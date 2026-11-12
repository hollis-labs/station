package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var (
	monitorBin string // the built binary, shared by the real-binary tests
	guardRoot  string // scratch HOME for the whole package (see TestMain)
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "usage-monitor-test")
	if err != nil {
		panic(err)
	}
	monitorBin = filepath.Join(dir, "usage-monitor")
	if out, err := exec.Command("go", "build", "-o", monitorBin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build usage-monitor: %v\n%s", err, out)
		os.Exit(2)
	}
	// A run without USAGE_MONITOR_DATA_DIR resolves the XDG data home of app `usage-monitor`, a live
	// directory on a real machine. Point every location at a scratch root for the whole package, so a
	// test (or a command it starts) that forgets the variable cannot reach it; TestDefaultDataDirIsNotLive
	// proves it. The catalog's default cache directory is under HOME too.
	guardRoot = filepath.Join(dir, "home")
	for k, v := range map[string]string{"HOME": guardRoot, "XDG_DATA_HOME": filepath.Join(guardRoot, "data"),
		"XDG_STATE_HOME": filepath.Join(guardRoot, "state"), "XDG_CONFIG_HOME": filepath.Join(guardRoot, "config"),
		"XDG_CACHE_HOME": filepath.Join(guardRoot, "cache")} {
		_ = os.Setenv(k, v)
	}
	_ = os.Unsetenv("USAGE_MONITOR_DATA_DIR")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// unit is one throwaway gateway: its own data directory, config override and listen address.
type unit struct {
	t    *testing.T
	dir  string
	cfg  string
	addr string
	env  []string
}

// newUnit prepares a data directory under t.TempDir and a SUBSTRATE_CONFIG that binds a free
// loopback port. Every run uses an explicit temporary data directory: the default one is live.
func newUnit(t *testing.T, extraConfig string) *unit {
	t.Helper()
	tmp := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	u := &unit{t: t, dir: filepath.Join(tmp, "data"), cfg: filepath.Join(tmp, "override.yaml"), addr: addr}
	body := fmt.Sprintf("httphost: {addr: %q}\nidentity: {bcrypt_cost: 4}\n%s", addr, extraConfig)
	if err := os.WriteFile(u.cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	u.env = append(os.Environ(), "USAGE_MONITOR_DATA_DIR="+u.dir, "SUBSTRATE_CONFIG="+u.cfg)
	return u
}

func (u *unit) command(stdin string, args ...string) *exec.Cmd {
	c := exec.Command(monitorBin, args...)
	c.Env = u.env
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	return c
}

// run runs the binary to completion and returns stdout and stderr together with the error.
func (u *unit) run(stdin string, args ...string) (string, error) {
	out, err := u.command(stdin, args...).CombinedOutput()
	return string(out), err
}

func (u *unit) mustRun(stdin string, args ...string) string {
	u.t.Helper()
	out, err := u.run(stdin, args...)
	if err != nil {
		u.t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}
