// Package vault: per-user at-rest encryption for chat history and uploaded
// documents. The data key (DEK) is wrapped with a wrap key that the browser
// derives from the user's login password (PBKDF2, in the browser — the
// gateway only ever sees the derived 32-byte wrap key, and it stores only a
// one-way hash of the password). So a stolen data.db + users.json + even the
// password hash cannot be unwrapped: only someone who KNOWS the password can.
//
// Envelope format (same shape as internal/auth's enc:v1:):
//
//	"vault:v1:" + base64.RawStd(nonce(12) || AES-256-GCM(tag16))
//
// Plaintext (legacy, pre-enable) values have no prefix and pass through
// unchanged — detection is by prefix, so a copy of the database alone
// reveals which values are sealed and nothing more.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	// Prefix: seals a stored value; the only on-disk marker of encryption.
	Prefix = "vault:v1:"
	// KDF parameters the BROWSER uses (WebCrypto PBKDF2-SHA256) to derive
	// the wrap key from the login password. Kept here for the JS + docs;
	// the Go side never runs the KDF. Must match vault.js.
	PBKDF2Iterations = 100_000
	Hash             = "SHA-256"
	// KeySize: wrap key and DEK are both AES-256 keys.
	KeySize = 32
)

var (
	// ErrLocked: the user's DEK is not cached in this process.
	ErrLocked = errors.New("vault is locked")
	// ErrWrongKey: the wrap key does not open the stored wrapped DEK
	// (wrong password, or the wrong user's key).
	ErrWrongKey = errors.New("wrong key")
)

// Envelope seals/opens stored values with one DEK.
type Envelope struct{ aead cipher.AEAD }

// NewEnvelope builds the AEAD for dek (must be 32 bytes).
func NewEnvelope(dek []byte) (*Envelope, error) {
	if len(dek) != KeySize {
		return nil, fmt.Errorf("vault: DEK must be %d bytes, got %d", KeySize, len(dek))
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Envelope{aead: aead}, nil
}

// Seal returns the sealed form of plain. Empty and already-sealed values
// pass through untouched (idempotent).
func (e *Envelope) Seal(plain string) string {
	if plain == "" || strings.HasPrefix(plain, Prefix) {
		return plain
	}
	nonce := make([]byte, e.aead.NonceSize())
	_, _ = rand.Read(nonce)
	ct := e.aead.Seal(nonce, nonce, []byte(plain), nil)
	return Prefix + base64.RawStdEncoding.EncodeToString(ct)
}

// Open decrypts v. A value without the prefix (legacy plaintext, or an
// empty string) is returned as-is; a wrong DEK fails the GCM tag.
func (e *Envelope) Open(v string) (string, error) {
	if v == "" || !strings.HasPrefix(v, Prefix) {
		return v, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(v[len(Prefix):])
	if err != nil || len(raw) < e.aead.NonceSize() {
		return "", errors.New("malformed vault value")
	}
	pt, err := e.aead.Open(nil, raw[:e.aead.NonceSize()], raw[e.aead.NonceSize():], nil)
	if err != nil {
		return "", ErrWrongKey
	}
	return string(pt), nil
}

// IsSealed reports whether v is a sealed value (prefix present).
func IsSealed(v string) bool { return strings.HasPrefix(v, Prefix) }

// WrapDEK seals a freshly generated DEK with wrapKey (browser-derived) so
// it can be stored in the vault table.
func WrapDEK(wrapKey, dek []byte) ([]byte, error) {
	if len(wrapKey) != KeySize || len(dek) != KeySize {
		return nil, fmt.Errorf("vault: wrap key and DEK must be %d bytes", KeySize)
	}
	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	return aead.Seal(nonce, nonce, dek, nil), nil
}

// UnwrapDEK opens a stored wrapped DEK with wrapKey. A wrong wrap key fails
// the GCM tag (constant-time in the AEAD) and returns ErrWrongKey.
func UnwrapDEK(wrapKey, wrapped []byte) ([]byte, error) {
	if len(wrapKey) != KeySize {
		return nil, fmt.Errorf("vault: wrap key must be %d bytes", KeySize)
	}
	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < aead.NonceSize() {
		return nil, ErrWrongKey
	}
	dk, err := aead.Open(nil, wrapped[:aead.NonceSize()], wrapped[aead.NonceSize():], nil)
	if err != nil {
		return nil, ErrWrongKey
	}
	return dk, nil
}

// Random returns n random bytes (panics on RNG failure, as crypto use does).
func Random(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
