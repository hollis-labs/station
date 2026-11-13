package main

import (
	"context"
	"fmt"
	"github.com/hollis-labs/station/internal/appboot"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/boot"
)

// The framework's bind check accepts every address in enforce mode, so refusing a
// non-loopback httphost.addr is Station's rule. It must hold before anything listens
// and before any logical server is spawned.
func TestServeRefusesNonLoopbackBind(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "[::]:0", ":0", "192.0.2.5:0", "100.64." + "0.5:0"} {
		t.Run(addr, func(t *testing.T) {
			override := fmt.Sprintf("httphost: {addr: %q}\nidentity: {bcrypt_cost: 4}\n", addr)
			file := filepath.Join(t.TempDir(), "override.yaml")
			if err := os.WriteFile(file, []byte(override), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("STATION_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			t.Setenv("SUBSTRATE_CONFIG", file)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			run, err := boot.StartWith(ctx, substrateApp, substrateModules, substrateConfigJSON, appboot.Options(os.Getenv, substrateModules, appboot.ModeCreate))
			if err != nil {
				t.Fatal(err)
			}
			defer run.Stop(context.Background()) //nolint:errcheck
			listened := false
			s := &station{app: run.App, ready: func(net.Addr) { listened = true }}
			err = s.serve(ctx)
			if err == nil || !strings.Contains(err.Error(), "loopback") {
				t.Fatalf("serve error = %v, want a loopback refusal", err)
			}
			if listened {
				t.Fatal("serve listened before refusing the address")
			}
		})
	}
}
