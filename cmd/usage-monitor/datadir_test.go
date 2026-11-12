package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/hollis-labs/substrate/boot"

	"github.com/hollis-labs/station/internal/appboot"
	"github.com/hollis-labs/station/internal/appdirs"
)

// Without USAGE_MONITOR_DATA_DIR the data directory is the XDG data home of the app name, which is a
// live directory on a real machine. TestMain points HOME and the XDG locations at a scratch root;
// this proves a run without the variable resolves inside it.
func TestDefaultDataDirIsNotLive(t *testing.T) {
	if os.Getenv(appdirs.DataDirEnv(substrateApp)) != "" {
		t.Fatalf("%s is set for the whole package; tests must set an explicit temporary directory themselves", appdirs.DataDirEnv(substrateApp))
	}
	dirs, err := appdirs.Resolve(substrateApp, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dirs.Data, guardRoot+string(filepath.Separator)) {
		t.Fatalf("a run without %s would use %s, outside the scratch root", appdirs.DataDirEnv(substrateApp), dirs.Data)
	}
	if got := appdirs.DataDirEnv("usage-monitor"); got != "USAGE_MONITOR_DATA_DIR" {
		t.Fatalf("env name %q", got)
	}
}

func TestPlan(t *testing.T) {
	all := substrateModules
	pick := func(want []string) []string { return appboot.Pick(all, want) }
	for _, tc := range []struct {
		args    []string
		modules []string
		prepare bool
	}{
		{nil, all, true},
		{[]string{"serve"}, all, true},
		{[]string{"serve", "--help"}, pick(appboot.ReadOnly), false},
		{[]string{"--help"}, pick(appboot.ReadOnly), false},
		{[]string{"help"}, pick(appboot.ReadOnly), false},
		{[]string{"completion", "bash"}, pick(appboot.ReadOnly), false},
		{[]string{"identity", "setup", "--username", "owner"}, pick(appboot.WithIdentity), true},
		{[]string{"identity", "key", "create", "--name", "n", "--scope", "s", "--stdout"}, pick(appboot.WithIdentity), true},
		{[]string{"identity"}, pick(appboot.ReadOnly), false},
		{[]string{"secret", "check", "file://a/b"}, pick(appboot.WithSecrets), false},
		{[]string{"secret", "set", "file://a/b"}, pick(appboot.WithSecrets), true},
		{[]string{"validate"}, pick(appboot.WithDatabaseSecrets), false},
		{[]string{"migrate-from", "--old-db", "/x/usage.db"}, pick(appboot.WithDatabase), true},
		{[]string{"migrate-from"}, pick(appboot.WithDatabase), true},
		{[]string{"nonsense"}, pick(appboot.ReadOnly), false},
	} {
		mods, prepare := plan(tc.args)
		if !slices.Equal(mods, tc.modules) || prepare != tc.prepare {
			t.Errorf("plan(%v) = %v, %v; want %v, %v", tc.args, mods, prepare, tc.modules, tc.prepare)
		}
	}
}

// Commands that only read start no module that creates anything: with an empty HOME and no
// USAGE_MONITOR_DATA_DIR, nothing appears. A reading command on a data directory with no database
// fails and still creates nothing. `secret set` is the positive control, so the emptiness check is
// known to be able to see a write.
func TestReadOnlyCommandsCreateNothing(t *testing.T) {
	run := func(home, stdin string, args ...string) (string, error) {
		c := newUnit(t, "").command(stdin, args...)
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_DATA_HOME=" + home + "/data",
			"XDG_STATE_HOME=" + home + "/state", "XDG_CONFIG_HOME=" + home + "/config"}
		out, err := c.CombinedOutput()
		return string(out), err
	}
	entries := func(home string) []string {
		var got []string
		_ = filepath.WalkDir(home, func(p string, _ fs.DirEntry, _ error) error {
			if p != home {
				got = append(got, p)
			}
			return nil
		})
		return got
	}
	for _, tc := range []struct {
		args    []string
		wantErr string // "" means the command must succeed
	}{
		{[]string{"--help"}, ""},
		{[]string{"help"}, ""},
		{[]string{"secret", "check", "file://ns/missing"}, "not_found"},
		{[]string{"validate"}, "no database"},
	} {
		home := t.TempDir()
		out, err := run(home, "", tc.args...)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%v failed: %v\n%s", tc.args, err, out)
		case tc.wantErr != "" && (err == nil || !strings.Contains(strings.ToLower(out), tc.wantErr)):
			t.Errorf("%v: err=%v output=%q, want a failure mentioning %q", tc.args, err, out, tc.wantErr)
		}
		if got := entries(home); len(got) != 0 {
			t.Errorf("%v created %v", tc.args, got)
		}
	}
	home := t.TempDir()
	if out, err := run(home, "value\n", "secret", "set", "file://ns/key"); err != nil {
		t.Fatalf("control: secret set failed: %v\n%s", err, out)
	}
	if len(entries(home)) == 0 {
		t.Fatal("control: secret set wrote nothing, so the emptiness check proves nothing")
	}
}

// plan treats every argument that does not start with "-" as a command word, which is right only
// while no root-level flag takes a value. Fails when one is added.
func TestRootHasNoValueFlags(t *testing.T) {
	t.Setenv("USAGE_MONITOR_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("SUBSTRATE_CONFIG", "")
	modules, prepare := plan([]string{"--help"})
	run, err := boot.StartWith(context.Background(), substrateApp, modules, substrateConfigJSON, appboot.Options(os.Getenv, modules, prepare))
	if err != nil {
		t.Fatal(err)
	}
	defer run.Stop(context.Background()) //nolint:errcheck
	root := (&monitor{app: run.App}).root(context.Background(), nil)
	check := func(fs *pflag.FlagSet) {
		fs.VisitAll(func(f *pflag.Flag) {
			if f.Value.Type() != "bool" {
				t.Errorf("root flag --%s takes a value (%s); plan() would read its value as the command word, teach it first", f.Name, f.Value.Type())
			}
		})
	}
	check(root.Flags())
	check(root.PersistentFlags())
}
