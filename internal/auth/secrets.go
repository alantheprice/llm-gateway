package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Secrets at rest: users.json holds the session signing secret, each
// user's plaintext UI key and connector auth headers. They're stored
// encrypted (AES-256-GCM) with a key kept in a separate file, so a copy of
// users.json alone (a backup, a stray commit) reveals none of them.
//
// Key file: <users.json>.key (0600), or $LLM_GATEWAY_SECRETS_KEY_FILE.
// Created on first use. Plain values from older files still load and are
// encrypted on the next save.

const encPrefix = "enc:v1:"

// SecretsKeyPath: where the encryption key for usersPath lives.
func SecretsKeyPath(usersPath string) string {
	if p := os.Getenv("LLM_GATEWAY_SECRETS_KEY_FILE"); p != "" {
		return p
	}
	return usersPath + ".key"
}

// loadOrCreateKey reads the 32-byte key (hex), creating it when absent.
// It refuses to create one while the users file already holds encrypted
// values: those would become unreadable.
func loadOrCreateKey(path string, haveEncrypted bool) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("secrets key %s is not 64 hex characters", path)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("secrets key: %w", err)
	}
	if haveEncrypted {
		return nil, fmt.Errorf("users.json has encrypted secrets but the key file %s is missing; restore it (or set LLM_GATEWAY_SECRETS_KEY_FILE)", path)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("secrets key: %w", err)
	}
	return key, nil
}

type sealer struct{ aead cipher.AEAD }

func newSealer(key []byte) (*sealer, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead}, nil
}

func (s *sealer) seal(plain string) string {
	if plain == "" || strings.HasPrefix(plain, encPrefix) {
		return plain
	}
	nonce := make([]byte, s.aead.NonceSize())
	_, _ = rand.Read(nonce)
	ct := s.aead.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.RawStdEncoding.EncodeToString(ct)
}

// open decrypts v; plain (legacy) values pass through, reporting legacy.
func (s *sealer) open(v string) (plain string, legacy bool, err error) {
	if !strings.HasPrefix(v, encPrefix) {
		return v, v != "", nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(v[len(encPrefix):])
	if err != nil || len(raw) < s.aead.NonceSize() {
		return "", false, errors.New("malformed encrypted value")
	}
	pt, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], nil)
	if err != nil {
		return "", false, errors.New("can't decrypt a secret in users.json: wrong key file?")
	}
	return string(pt), false, nil
}

// hasEncrypted: does the raw users.json hold any encrypted value?
func hasEncrypted(data []byte) bool {
	return strings.Contains(string(data), encPrefix)
}

// decryptSecrets turns the stored (encrypted or legacy plain) secrets into
// plaintext in memory. Returns whether any legacy plain value was found.
func (s *Store) decryptSecrets() (legacy bool, err error) {
	if s.seal == nil {
		return false, nil
	}
	dec := func(v string) string {
		if err != nil {
			return v
		}
		p, l, e := s.seal.open(v)
		if e != nil {
			err = e
			return v
		}
		legacy = legacy || l
		return p
	}
	s.SessionSecret = dec(s.SessionSecret)
	for u, v := range s.AutoKeyPlain {
		s.AutoKeyPlain[u] = dec(v)
	}
	for u, list := range s.MCPServers {
		for i := range list {
			for k, v := range list[i].Headers {
				list[i].Headers[k] = dec(v)
			}
		}
		s.MCPServers[u] = list
	}
	return legacy, err
}

// marshalSealed: users.json bytes with the secrets encrypted. Caller holds s.mu.
func (s *Store) marshalSealed() ([]byte, error) {
	data, err := json.Marshal(s)
	if err != nil || s.seal == nil {
		return indent(data), err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	put := func(k string, v any) error {
		b, err := json.Marshal(v)
		if err == nil {
			doc[k] = b
		}
		return err
	}
	if err := put("session_secret", s.seal.seal(s.SessionSecret)); err != nil {
		return nil, err
	}
	if len(s.AutoKeyPlain) > 0 {
		m := make(map[string]string, len(s.AutoKeyPlain))
		for u, v := range s.AutoKeyPlain {
			m[u] = s.seal.seal(v)
		}
		if err := put("auto_key_plaintexts", m); err != nil {
			return nil, err
		}
	}
	if len(s.MCPServers) > 0 {
		m := make(map[string][]MCPServer, len(s.MCPServers))
		for u, list := range s.MCPServers {
			c := copyMCP(list)
			for i := range c {
				for k, v := range c[i].Headers {
					c[i].Headers[k] = s.seal.seal(v)
				}
			}
			m[u] = c
		}
		if err := put("mcp_servers", m); err != nil {
			return nil, err
		}
	}
	out, err := json.Marshal(doc)
	return indent(out), err
}

func indent(b []byte) []byte {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return b
	}
	return out
}
