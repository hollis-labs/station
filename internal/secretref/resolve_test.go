package secretref

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/mcp-host/config"
)

type fakeResolver map[string]string

func (f fakeResolver) Resolve(ctx context.Context, ref string) (string, error) {
	v, ok := f[ref]
	if !ok {
		return "", errors.New("no such reference")
	}
	return v, nil
}

func withFakeResolver(t *testing.T, f fakeResolver) {
	t.Helper()
	orig := newResolver
	newResolver = func() resolver { return f }
	t.Cleanup(func() { newResolver = orig })
}

func TestResolve_ReplacesReferenceValuedEnv(t *testing.T) {
	withFakeResolver(t, fakeResolver{"keychain://api-projection/github-pilot": "ghp_realtoken"})

	cfg := &config.Config{
		LogicalServers: []config.LogicalServer{
			{
				ID:        "github-pilot",
				Transport: config.TransportProcess,
				Process: &config.ProcessConfig{
					Command: "./bin/api-projection-server",
					Env:     map[string]string{"GITHUB_TOKEN": "keychain://api-projection/github-pilot"},
				},
			},
		},
	}

	if err := Resolve(context.Background(), cfg); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := cfg.LogicalServers[0].Process.Env["GITHUB_TOKEN"]; got != "ghp_realtoken" {
		t.Fatalf("GITHUB_TOKEN = %q, want the resolved value", got)
	}
}

func TestResolve_LeavesLiteralEnvUntouched(t *testing.T) {
	withFakeResolver(t, fakeResolver{})

	cfg := &config.Config{
		LogicalServers: []config.LogicalServer{
			{
				ID:        "echo",
				Transport: config.TransportProcess,
				Process: &config.ProcessConfig{
					Command: "./bin/echo-server",
					Env:     map[string]string{"MODE": "friendly"},
				},
			},
		},
	}

	if err := Resolve(context.Background(), cfg); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := cfg.LogicalServers[0].Process.Env["MODE"]; got != "friendly" {
		t.Fatalf("MODE = %q, want unchanged literal value", got)
	}
}

func TestResolve_UnresolvableReferenceIsFatal(t *testing.T) {
	withFakeResolver(t, fakeResolver{})

	cfg := &config.Config{
		LogicalServers: []config.LogicalServer{
			{
				ID:        "github-pilot",
				Transport: config.TransportProcess,
				Process: &config.ProcessConfig{
					Command: "./bin/api-projection-server",
					Env:     map[string]string{"GITHUB_TOKEN": "keychain://api-projection/does-not-exist"},
				},
			},
		},
	}

	if err := Resolve(context.Background(), cfg); err == nil {
		t.Fatal("Resolve should fail rather than spawn with a blank credential")
	}
}

func TestResolve_SkipsInprocessAndDialLogicalServers(t *testing.T) {
	withFakeResolver(t, fakeResolver{})

	cfg := &config.Config{
		LogicalServers: []config.LogicalServer{
			{ID: "clock", Transport: config.TransportInprocess, Inprocess: &config.InprocessConfig{Command: "./bin/clock-plugin", Tools: []config.ToolManifest{{Name: "now"}}}},
			{ID: "dialed", Transport: config.TransportProcess, Process: &config.ProcessConfig{URL: "http://example.invalid"}},
		},
	}

	if err := Resolve(context.Background(), cfg); err != nil {
		t.Fatalf("Resolve should not touch inprocess or dial-mode servers: %v", err)
	}
}
