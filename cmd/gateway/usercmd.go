// CLI management modes: superuser passthrough + first-admin bootstrap.
package main

import (
	"fmt"

	"llmgateway/internal/auth"
	"llmgateway/internal/embeddedpb"
)

// userCmd: `llm-gateway user create-admin <username> [password]` —
// provisions the first admin (PB account + role=admin) non-interactively,
// for install scripts. Upserts: safe to re-run.
func userCmd(dataDir string, port int, args []string) error {
	if len(args) < 2 || args[0] != "create-admin" {
		return fmt.Errorf("usage: llm-gateway user create-admin <username> [password]\n" +
			"  (password omitted → generated and printed once)")
	}
	username := args[1]
	password := ""
	if len(args) > 2 {
		password = args[2]
	}

	app, err := embeddedpb.BootstrapApp(dataDir, port)
	if err != nil {
		return err
	}
	defer app.Close()
	if err := app.CreateAdminUser(username, password); err != nil {
		return err
	}
	fmt.Printf("Admin %q created (role=admin).\n", username)
	if len(args) <= 2 {
		fmt.Printf("Password (shown once): %s\n", password)
	}
	envPath, err := app.EnsureSuperuserEnv(dataDir, "", "")
	if err != nil {
		fmt.Printf("note: PB superuser env not created: %v\n", err)
		return nil
	}
	fmt.Printf("PB dashboard superuser written to %s (dashboard: http://127.0.0.1:%d/_/)\n", envPath, port)
	return nil
}

// keyCmd: `llm-gateway key create <username> <key_id> [role]` — mints an
// API key into users.json and prints the plaintext once. For link tokens
// use role "link" (see docs/link-agent.md). Safe while the gateway runs:
// it re-reads users.json within a few seconds of an external write.
func keyCmd(usersPath string, args []string) error {
	if len(args) < 3 || args[0] != "create" {
		return fmt.Errorf("usage: llm-gateway key create <username> <key_id> [role]\n" +
			"  role: \"link\" for link-agent tokens; empty for a normal key")
	}
	username, keyID, role := args[1], args[2], ""
	if len(args) > 3 {
		role = args[3]
	}
	store, err := auth.Open(usersPath)
	if err != nil {
		return err
	}
	plain, rec, err := store.CreateKey(username, keyID, role, false)
	if err != nil {
		return err
	}
	fmt.Printf("Key %q created for %s (role=%q).\n", rec.KeyID, username, rec.Role)
	fmt.Printf("Plaintext (shown once): %s\n", plain)
	return nil
}
