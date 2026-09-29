package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretsEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	// An older users.json with plain secrets.
	legacy := `{"session_secret":"plain-session-secret","auto_key_plaintexts":{"carol":"sk-plain-ui-key"},
	  "mcp_servers":{"carol":[{"id":"a","name":"gh","url":"https://x","headers":{"Authorization":"Bearer ghp_topsecret"},"enabled":true}]},
	  "local_keys":{},"must_change_pw":{},"session_epochs":{}}`
	os.WriteFile(path, []byte(legacy), 0o600)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionSecret != "plain-session-secret" || s.MCPServersOf("carol")[0].Headers["Authorization"] != "Bearer ghp_topsecret" {
		t.Fatalf("in-memory values: %q %v", s.SessionSecret, s.MCPServersOf("carol"))
	}
	raw, _ := os.ReadFile(path)
	for _, secret := range []string{"plain-session-secret", "sk-plain-ui-key", "ghp_topsecret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%q still in plain text on disk:\n%s", secret, raw)
		}
	}
	if fi, err := os.Stat(SecretsKeyPath(path)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}

	// Reopening decrypts the same values.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.SessionSecret != "plain-session-secret" || s2.AutoKeyPlain["carol"] != "sk-plain-ui-key" ||
		s2.MCPServersOf("carol")[0].Headers["Authorization"] != "Bearer ghp_topsecret" {
		t.Fatal("secrets didn't round-trip")
	}

	// Without the key, opening fails clearly instead of losing the secrets.
	os.Rename(SecretsKeyPath(path), SecretsKeyPath(path)+".bak")
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "key file") {
		t.Fatalf("missing key: %v", err)
	}
	// A wrong key is refused too.
	os.WriteFile(SecretsKeyPath(path), []byte(strings.Repeat("ab", 32)), 0o600)
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("wrong key: %v", err)
	}
}
