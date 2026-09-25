// Package auth implements users.json storage, PBKDF2 key hashing, session
// tokens, and legacy key files — byte-compatible with the Python gateway.
// See docs/SPEC.md §1.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const PBKDF2Iterations = 60000

// HashSecret returns hex(pbkdf2_hmac_sha256(secret, salt, 60000)) —
// identical to Python's _hash_secret.
func HashSecret(secret, salt string) string {
	return hex.EncodeToString(pbkdf2SHA256([]byte(secret), []byte(salt), PBKDF2Iterations, 32))
}

func hashEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// KeyRecord mirrors one entry of users.json local_keys[username][].
type KeyRecord struct {
	KeyID      string  `json:"key_id"`
	Prefix     string  `json:"prefix"`
	Salt       string  `json:"salt"`
	KeyHash    string  `json:"key_hash"`
	Created    string  `json:"created"`
	Active     bool    `json:"active"`
	Role       string  `json:"role,omitempty"`
	Rotating   bool    `json:"rotating"`
	GraceUntil *string `json:"grace_until"` // ISO datetime string (Python parity)
	UI         bool    `json:"ui,omitempty"`
}

const isoLayout = "2006-01-02T15:04:05"

func (k *KeyRecord) usable(now time.Time) bool {
	if k.Active {
		return true
	}
	if k.Rotating && k.GraceUntil != nil {
		if until, err := time.ParseInLocation(isoLayout, *k.GraceUntil, time.Local); err == nil {
			return now.Before(until)
		}
	}
	return false
}

// ExpireRotations flips rotating keys past their grace window to inactive
// (in place). Returns true if anything changed (caller persists).
func ExpireRotations(s *Store) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	now := time.Now()
	for _, keys := range s.LocalKeys {
		for _, k := range keys {
			if k.Rotating && k.GraceUntil != nil {
				if until, err := time.ParseInLocation(isoLayout, *k.GraceUntil, time.Local); err == nil && now.After(until) {
					k.Active, k.Rotating, k.GraceUntil = false, false, nil
					changed = true
				}
			}
		}
	}
	if changed {
		_ = s.saveLocked()
	}
	return changed
} // Store is the users.json document plus its lock.
type Store struct {
	mu   sync.Mutex
	path string

	SessionSecret   string                  `json:"session_secret"`
	LocalKeys       map[string][]*KeyRecord `json:"local_keys"`
	MustChangePWMap map[string]bool         `json:"must_change_pw"`
	SessionEpochs   map[string]int          `json:"session_epochs"`
	// UserSettings: gateway-owned per-user policy (daily quotas etc.).
	// 0 limit = unlimited.
	UserSettings map[string]UserSettings `json:"user_settings,omitempty"`

	// legacy key file path ("" disables)
	LegacyKeysFile string
	legacyKeys     []string
	legacyAt       time.Time
}

// UserSettings carries per-user service policy.
type UserSettings struct {
	DailyTokenLimit int `json:"daily_token_limit"` // prompt+output per UTC day; 0 = unlimited
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, LocalKeys: map[string][]*KeyRecord{},
		MustChangePWMap: map[string]bool{}, SessionEpochs: map[string]int{}}
	if err := s.reload(); err != nil {
		return nil, err
	}
	if s.SessionSecret == "" {
		s.SessionSecret = newSecret(32)
	}
	// Poll for external writes: two gateways sharing one users.json (the
	// drop-in/cutover scenario) must see each other's key churn. Python's
	// _load_users re-reads on a 30s TTL; we watch mtime every 5s.
	go s.watchLoop()
	return s, nil
}

// reload re-reads users.json from disk. Caller holds s.mu (or is Open).
func (s *Store) reload() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		s.SessionSecret = newSecret(32)
		return nil
	}
	if err := json.Unmarshal(data, s); err != nil {
		return fmt.Errorf("users.json parse: %w", err)
	}
	if s.LocalKeys == nil {
		s.LocalKeys = map[string][]*KeyRecord{}
	}
	if s.SessionEpochs == nil {
		s.SessionEpochs = map[string]int{}
	}
	if s.MustChangePWMap == nil {
		s.MustChangePWMap = map[string]bool{}
	}
	if s.UserSettings == nil {
		s.UserSettings = map[string]UserSettings{}
	}
	return nil
}

// DailyLimit returns the user's daily token limit (0 = unlimited).
func (s *Store) DailyLimit(username string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.UserSettings[username].DailyTokenLimit
}

// SetDailyLimit persists a user's daily token limit (0 = unlimited).
func (s *Store) SetDailyLimit(username string, limit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 0 {
		limit = 0
	}
	if s.UserSettings == nil {
		s.UserSettings = map[string]UserSettings{}
	}
	if limit == 0 {
		delete(s.UserSettings, username)
	} else {
		s.UserSettings[username] = UserSettings{DailyTokenLimit: limit}
	}
	return s.saveLocked()
}

// watchLoop re-reads users.json when its mtime advances (external writers).
func (s *Store) watchLoop() {
	var last time.Time
	if fi, err := os.Stat(s.path); err == nil {
		last = fi.ModTime()
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		fi, err := os.Stat(s.path)
		if err != nil || !fi.ModTime().After(last) {
			continue
		}
		s.mu.Lock()
		if err := s.reload(); err == nil {
			last = fi.ModTime()
		}
		s.mu.Unlock()
	}
}

// Save atomically writes users.json with mode 0600 (SPEC §1.2).
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Path() string { return s.path }

func newSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// --- key management ---

// NewAPIKey generates (plaintext, prefix, salt) with Python parity:
// plaintext = 'sk-' + token_urlsafe(30), prefix = plaintext[:11],
// salt = prefix + 'salt'. HashSecret(plaintext, salt) gives key_hash.
func NewAPIKey() (plaintext, prefix, salt string) {
	plaintext = "sk-" + tokenURLSafe(30)
	prefix = plaintext[:11]
	salt = prefix + "salt"
	return
}

func tokenURLSafe(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)[:n]
}

// CreateKey mints a new plaintext key, stores its hash, and returns the
// plaintext (shown once) along with the record. Python parity: prefix/salt
// scheme, key_id trimmed to 40 chars.
func (s *Store) CreateKey(username, keyID, role string, ui bool) (string, *KeyRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	plain, prefix, salt := NewAPIKey()
	if len(keyID) > 40 {
		keyID = keyID[:40]
	}
	if keyID == "" {
		keyID = "key"
	}
	rec := &KeyRecord{
		KeyID:   keyID,
		Prefix:  prefix,
		Salt:    salt,
		Created: now.Format(isoLayout),
		Active:  true,
		Role:    role,
		UI:      ui,
	}
	rec.KeyHash = HashSecret(plain, rec.Salt)
	s.LocalKeys[username] = append(s.LocalKeys[username], rec)
	if err := s.saveLocked(); err != nil {
		return "", nil, err
	}
	return plain, rec, nil
}

// RotateKey retires keyID (keeping it valid for grace seconds under a
// -retired- name) and mints a replacement with the same id. Python parity.
func (s *Store) RotateKey(username, keyID string, graceSeconds int) (string, *string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.LocalKeys[username] {
		if k.KeyID == keyID && k.Active {
			plain, prefix, salt := NewAPIKey()
			retiredID := keyID + "-retired-" + last4(k.Prefix)
			grace := time.Now().Add(time.Duration(graceSeconds) * time.Second).Format(isoLayout)
			k.KeyID = retiredID
			k.Rotating = true
			k.Active = false
			k.GraceUntil = &grace
			newRec := &KeyRecord{
				KeyID: keyID, Prefix: prefix, Salt: salt,
				Created: time.Now().UTC().Format(isoLayout), Active: true,
			}
			newRec.KeyHash = HashSecret(plain, salt)
			s.LocalKeys[username] = append(s.LocalKeys[username], newRec)
			if err := s.saveLocked(); err != nil {
				return "", nil, err
			}
			return plain, &grace, nil
		}
	}
	return "", nil, fmt.Errorf("no such active key")
}

func last4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}

// CountActiveKeys returns the user's active (non-rotating) key count.
func (s *Store) CountActiveKeys(username string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, k := range s.LocalKeys[username] {
		if k.Active && !k.Rotating {
			n++
		}
	}
	return n
}

// LookupKey resolves a plaintext key to (username, record). Expired rotations
// are lazily expired (SPEC §1.2). Returns ok=false when unknown/disabled.
func (s *Store) LookupKey(plain string) (string, *KeyRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for username, keys := range s.LocalKeys {
		for _, k := range keys {
			if !k.usable(now) {
				continue
			}
			if hashEq(k.KeyHash, HashSecret(plain, k.Salt)) {
				return username, k, true
			}
		}
	}
	return "", nil, false
}

// RevokeKey removes a key by id (and any of its -retired- rotations).
func (s *Store) RevokeKey(username, keyID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := s.LocalKeys[username]
	out := keys[:0]
	found := false
	for _, k := range keys {
		if k.KeyID == keyID || strings.HasPrefix(k.KeyID, keyID+"-retired-") {
			found = true
			continue
		}
		out = append(out, k)
	}
	if !found {
		return false
	}
	s.LocalKeys[username] = out
	_ = s.saveLocked()
	return true
}

// BumpEpoch invalidates all existing session cookies for username (SPEC §1.3).
func (s *Store) BumpEpoch(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SessionEpochs[username]++
	_ = s.saveLocked()
}

func (s *Store) Epoch(username string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.SessionEpochs[username]
}

// --- legacy keys file ---

func (s *Store) LegacyKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.LegacyKeysFile == "" {
		return nil
	}
	if fi, err := os.Stat(s.LegacyKeysFile); err == nil {
		if !fi.ModTime().After(s.legacyAt) && s.legacyAt.IsZero() == false {
			// cached
		} else {
			data, err := os.ReadFile(s.LegacyKeysFile)
			if err == nil {
				var out []string
				for _, line := range strings.Split(string(data), "\n") {
					line = strings.TrimSpace(line)
					if line == "" || strings.HasPrefix(line, "#") {
						continue
					}
					out = append(out, line)
				}
				s.legacyKeys = out
				s.legacyAt = time.Now()
			}
		}
	}
	return s.legacyKeys
}

// --- session tokens (SPEC §1.3, byte-compatible with Python _HmacSigner) ---

type Claims struct {
	U    string `json:"u"`
	Role string `json:"role"`
	Ep   int    `json:"ep,omitempty"`
	MCP  bool   `json:"mcp,omitempty"`
}

// SignSession builds token = b64url(json(claims)+"|"+exp) + "." + hmac_hex.
// JSON serialization matches Python json.dumps (", " / ": " separators).
func (s *Store) SignSession(c Claims, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	return s.SignSessionOrdered(exp, KV{"u", c.U}, KV{"role", c.Role}, KV{"ep", c.Ep})
}

// KV is one ordered claim (Python dicts preserve insertion order; Go maps
// don't, so callers pass pairs).
type KV struct {
	K string
	V any
}

// SignSessionOrdered signs ordered claims with a fixed expiry (golden tests,
// cross-language parity). Serialized exactly like Python json.dumps defaults.
func (s *Store) SignSessionOrdered(exp int64, kvs ...KV) string {
	body := pyDumpsOrdered(kvs) + "|" + fmt.Sprint(exp)
	mac := hmac.New(sha256.New, []byte(s.SessionSecret))
	mac.Write([]byte(body))
	return base64.URLEncoding.EncodeToString([]byte(body)) + "." + hex.EncodeToString(mac.Sum(nil))
}

// VerifySession validates signature, expiry, and epoch. Returns claims.
func (s *Store) VerifySession(token string) (Claims, bool) {
	var c Claims
	dot := strings.LastIndex(token, ".")
	if dot < 0 {
		return c, false
	}
	bodyB64, sigHex := token[:dot], token[dot+1:]
	body, err := base64.URLEncoding.DecodeString(bodyB64)
	if err != nil {
		return c, false
	}
	mac := hmac.New(sha256.New, []byte(s.SessionSecret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sigHex), []byte(want)) != 1 {
		return c, false
	}
	bar := strings.LastIndex(string(body), "|")
	if bar < 0 {
		return c, false
	}
	var exp int64
	if _, err := fmt.Sscanf(string(body[bar+1:]), "%d", &exp); err != nil {
		return c, false
	}
	if time.Now().Unix() >= exp {
		return c, false
	}
	if err := json.Unmarshal([]byte(body[:bar]), &c); err != nil {
		return c, false
	}
	if c.Ep != s.Epoch(c.U) {
		return c, false
	}
	return c, true
}

// pyDumpsOrdered serializes ordered claims like Python json.dumps defaults:
// ", " between items, ": " after keys. Scalar encodings match for the types
// sessions use.
func pyDumpsOrdered(kvs []KV) string {
	var b strings.Builder
	b.WriteString("{")
	for i, kv := range kvs {
		if i > 0 {
			b.WriteString(", ")
		}
		kb, _ := json.Marshal(kv.K)
		b.Write(kb)
		b.WriteString(": ")
		b.WriteString(pyScalar(kv.V))
	}
	b.WriteString("}")
	return b.String()
}

func pyScalar(v any) string {
	switch x := v.(type) {
	case string:
		b, _ := json.Marshal(x) // JSON escaping == Python's for session claims
		return string(b)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprint(x)
	case int64:
		return fmt.Sprint(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// pbkdf2SHA256 is a minimal PBKDF2 (RFC 2898) with HMAC-SHA256.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	var buf [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	U := make([]byte, hashLen)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf[:4])
		dk = prf.Sum(dk)
		T := dk[len(dk)-hashLen:]
		copy(U, T)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(U)
			U = U[:0]
			U = prf.Sum(U)
			for i := range U {
				T[i] ^= U[i]
			}
		}
	}
	return dk[:keyLen]
}
