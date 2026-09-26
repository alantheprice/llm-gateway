// CLI management modes: superuser passthrough + first-admin bootstrap.
package main

import (
	"fmt"

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
