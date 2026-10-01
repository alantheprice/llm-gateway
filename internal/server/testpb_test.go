package server

import (
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"llmgateway/internal/embeddedpb"
)

// testPB: one real embedded PocketBase per test process (accounts,
// analytics, ops and chat tables), started on first use on a free port.
var (
	pbOnce   sync.Once
	pbApp    *embeddedpb.App
	pbPortN  int
	pbSuPass string
	pbErr    error
)

func testPB(t *testing.T) (*embeddedpb.App, int, string) {
	t.Helper()
	pbOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gw-testpb-")
		if err != nil {
			pbErr = err
			return
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			pbErr = err
			return
		}
		pbPortN = l.Addr().(*net.TCPAddr).Port
		l.Close()
		if pbApp, pbErr = embeddedpb.Start(embeddedpb.Config{DataDir: dir, Port: pbPortN}); pbErr != nil {
			return
		}
		if pbErr = pbApp.WaitUntilHealthy("127.0.0.1", pbPortN, 20*time.Second); pbErr != nil {
			return
		}
		for _, f := range []func() error{pbApp.InitOpsTables, pbApp.InitRequestsSchema, pbApp.InitChatsSchema, pbApp.InitDocsSchema, pbApp.InitMemoriesSchema} {
			if pbErr = f(); pbErr != nil {
				return
			}
		}
		if pbSuPass, pbErr = pbApp.EnsureSuperuserEnv(dir, "admin@llm.local", "page-test-superuser"); pbErr != nil {
			return
		}
		for _, u := range []string{"admin", "carol"} {
			if pbErr = pbApp.CreateAdminUser(u, "page-test-password"); pbErr != nil {
				return
			}
		}
	})
	if pbErr != nil {
		t.Fatalf("embedded PocketBase: %v", pbErr)
	}
	return pbApp, pbPortN, pbSuPass
}
