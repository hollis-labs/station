package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	mcpconfig "github.com/hollis-labs/libs/mcp/plugin-mcp/mcp-host/config"
	libsecrets "github.com/hollis-labs/libs/security/secrets"
)

type fakeResolver map[string]string

func (f fakeResolver) Resolve(_ context.Context, ref string) (libsecrets.Resolved, error) {
	v, ok := f[ref]
	if !ok {
		return libsecrets.Resolved{}, errors.New("not found")
	}
	return libsecrets.Resolved{Secret: libsecrets.FromString(v)}, nil
}

func cfg() *mcpconfig.Config {
	return &mcpconfig.Config{LogicalServers: []mcpconfig.LogicalServer{{
		ID: "design", Transport: mcpconfig.TransportProcess,
		Process: &mcpconfig.ProcessConfig{Command: "/bin/true", Env: map[string]string{
			"PROVIDER_KEY": "keychain://stand-in/provider", "PLAIN": "value", "WORKFLOW_DIR": "/data/workflows",
		}},
	}}}
}

func TestResolveEnvResolvesReferencesIntoACopy(t *testing.T) {
	in := cfg()
	out, err := resolveEnv(context.Background(), in, fakeResolver{"keychain://stand-in/provider": "stand-in-value"})
	if err != nil {
		t.Fatal(err)
	}
	env := out.LogicalServers[0].Process.Env
	if env["PROVIDER_KEY"] != "stand-in-value" || env["PLAIN"] != "value" || env["WORKFLOW_DIR"] != "/data/workflows" {
		t.Fatalf("resolved env = %v", env)
	}
	if got := in.LogicalServers[0].Process.Env["PROVIDER_KEY"]; got != "keychain://stand-in/provider" {
		t.Fatalf("the input config was changed: %q", got)
	}
}

func TestResolveEnvFailsNamingTheKeyNotTheValue(t *testing.T) {
	_, err := resolveEnv(context.Background(), cfg(), fakeResolver{})
	if err == nil || !strings.Contains(err.Error(), "design") || !strings.Contains(err.Error(), "PROVIDER_KEY") {
		t.Fatalf("err = %v", err)
	}
	if _, err = resolveEnv(context.Background(), cfg(), nil); err == nil {
		t.Fatal("a reference with no resolver must fail, not spawn a child with the reference as its value")
	}
}
