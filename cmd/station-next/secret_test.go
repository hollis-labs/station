package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/boot"

	"github.com/hollis-labs/station/internal/appcmd"
)

func bootOnly(t *testing.T) *station {
	t.Helper()
	t.Setenv("STATION_DATA_DIR", filepath.Join(t.TempDir(), "data")) // never the live directory
	t.Setenv("SUBSTRATE_CONFIG", "")
	ctx, cancel := context.WithCancel(context.Background())
	run, err := boot.StartWith(ctx, substrateApp, substrateModules, substrateConfigJSON, boot.Options{AfterConfig: afterConfig(os.Getenv, true)})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = run.Stop(context.Background()) })
	return &station{app: run.App}
}

func TestSecretSetReadsStdinAndCheckNeverPrintsTheValue(t *testing.T) {
	s := bootOnly(t)
	const ref, value = "file://stand-in/provider-key", "stand-in-secret-value-0003"

	var out, errOut bytes.Buffer
	cmd := appcmd.Secret(s.app)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(value + "\n"))
	cmd.SetArgs([]string{"set", ref})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("set: %v", err)
	}
	if strings.Contains(out.String()+errOut.String(), value) {
		t.Fatal("set printed the value")
	}

	out.Reset()
	cmd = appcmd.Secret(s.app)
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"check", ref})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if !strings.Contains(out.String(), "resolves") || !strings.Contains(out.String(), "26 bytes") || strings.Contains(out.String(), value) {
		t.Fatalf("check output: %q", out.String())
	}

	// An empty stdin and an unknown reference are refused with a message that does not carry a value.
	cmd = appcmd.Secret(s.app)
	cmd.SetIn(strings.NewReader("\n"))
	cmd.SetArgs([]string{"set", "file://stand-in/empty"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "empty value") {
		t.Fatalf("empty stdin: %v", err)
	}
	cmd = appcmd.Secret(s.app)
	cmd.SetArgs([]string{"check", "file://stand-in/missing"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("a missing reference resolved")
	}
}
