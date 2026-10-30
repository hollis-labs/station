package platform

import (
	"io"
	"log/slog"
	"testing"

	"github.com/hollis-labs/libs/security/auth"
)

func dialMode(t *testing.T, cfg Config, app, gen string) (auth.Mode, error) {
	t.Helper()
	t.Setenv("HOLLIS_AUTH_STATION", app)
	t.Setenv("HOLLIS_AUTH", gen)
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	g, _, err := buildGuard(&cfg, NewRegistry(lg), lg)
	if err != nil {
		return "", err
	}
	return g.Mode(), nil
}

func TestAuthDial(t *testing.T) {
	lo := Config{Listen: "127.0.0.1:8081"}
	if m, err := dialMode(t, lo, "", ""); err != nil || m != auth.ModeEnforce {
		t.Fatalf("default: %v %v", m, err)
	}
	if m, err := dialMode(t, lo, "off", ""); err != nil || m != auth.ModeOff {
		t.Fatalf("off: %v %v", m, err)
	}
	if m, err := dialMode(t, lo, "", "permissive"); err != nil || m != auth.ModePermissive {
		t.Fatalf("permissive via HOLLIS_AUTH: %v %v", m, err)
	}
	if m, err := dialMode(t, lo, "enforce", "off"); err != nil || m != auth.ModeEnforce {
		t.Fatalf("app var must win: %v %v", m, err)
	}
	if m, err := dialMode(t, Config{Listen: "127.0.0.1:1", Auth: AuthConfig{Mode: "enforce"}}, "off", ""); err != nil || m != auth.ModeEnforce {
		t.Fatalf("explicit config wins: %v %v", m, err)
	}
	if _, err := dialMode(t, lo, "bogus", ""); err == nil {
		t.Fatal("invalid value accepted")
	}
	for _, l := range []string{"0.0.0.0:8081", ":8081", "100.64.0.5:8081"} {
		if _, err := dialMode(t, Config{Listen: l}, "off", ""); err == nil {
			t.Fatalf("off on %s must be refused", l)
		}
		if _, err := dialMode(t, Config{Listen: l}, "permissive", ""); err == nil {
			t.Fatalf("permissive on %s must be refused", l)
		}
		if m, err := dialMode(t, Config{Listen: l}, "enforce", ""); err != nil || m != auth.ModeEnforce {
			t.Fatalf("enforce on %s: %v %v", l, m, err)
		}
	}
}
