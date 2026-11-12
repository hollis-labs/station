package main

import (
	"context"
	"github.com/hollis-labs/station/internal/appboot"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/hollis-labs/substrate/boot"

	"github.com/hollis-labs/station/internal/appdirs"
)

// Without STATION_DATA_DIR the data directory is the XDG data home of the app name, which is a live
// directory on a real machine. TestMain moves HOME and the XDG variables to a scratch root; this
// fails if any run could still fall back to somewhere else.
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
}

func TestPlan(t *testing.T) {
	all := substrateModules
	pick := func(want []string) []string { return appboot.Pick(all, want) }
	ro := pick(appboot.ReadOnly)
	for _, tc := range []struct {
		args    []string
		modules []string
		mode    appboot.Mode
	}{
		{[]string{"serve"}, all, appboot.ModeServe},
		{nil, ro, appboot.ModeRead},
		{[]string{"--bogus"}, ro, appboot.ModeRead},
		{[]string{"--help=true"}, ro, appboot.ModeRead},
		{[]string{"serve", "--help"}, ro, appboot.ModeRead},
		{[]string{"serve", "--port", "1"}, ro, appboot.ModeRead},
		{[]string{"serve", "extra"}, ro, appboot.ModeRead},
		{[]string{"--help"}, ro, appboot.ModeRead},
		{[]string{"help"}, ro, appboot.ModeRead},
		{[]string{"completion", "bash"}, ro, appboot.ModeRead},
		{[]string{"identity", "setup", "--username", "owner"}, pick(appboot.WithIdentity), appboot.ModeCreate},
		{[]string{"identity", "setup"}, ro, appboot.ModeRead},
		{[]string{"identity", "setup", "--username"}, ro, appboot.ModeRead},
		{[]string{"identity", "key", "create", "--name", "n", "--scope", "s", "--stdout"}, pick(appboot.WithIdentity), appboot.ModeCreate},
		{[]string{"identity", "key", "create", "--name", "n", "--scope", "s"}, ro, appboot.ModeRead},
		{[]string{"identity", "key", "create", "--name", "n", "--scope", "s", "--out", "/x", "--stdout"}, ro, appboot.ModeRead},
		{[]string{"identity", "key", "create", "--scope", "s", "--stdout"}, ro, appboot.ModeRead},
		{[]string{"identity"}, ro, appboot.ModeRead},
		{[]string{"secret", "check", "file://a/b"}, pick(appboot.WithSecrets), appboot.ModeRead},
		{[]string{"secret", "set", "file://a/b"}, pick(appboot.WithSecrets), appboot.ModeCreate},
		{[]string{"secret", "set"}, ro, appboot.ModeRead},
		{[]string{"nonsense"}, ro, appboot.ModeRead},
		{[]string{"validate"}, ro, appboot.ModeRead},
		{[]string{"list"}, ro, appboot.ModeRead},
	} {
		mods, mode := plan(tc.args)
		if !slices.Equal(mods, tc.modules) || mode != tc.mode {
			t.Errorf("plan(%q) = %v, %v; want %v, %v", tc.args, mods, mode, tc.modules, tc.mode)
		}
	}
}

// Read-only commands start no module that opens a file and create no directory: with an empty
// HOME and no STATION_DATA_DIR, nothing appears. `secret set` is the positive control, so the
// emptiness check is known to be able to see a write.
func TestReadOnlyCommandsCreateNothing(t *testing.T) {
	tmp := t.TempDir()
	bin, fake := filepath.Join(tmp, "station"), filepath.Join(tmp, "fakechild")
	for target, pkg := range map[string]string{bin: ".", fake: "github.com/hollis-labs/station/cmd/station-next/testdata/fakechild"} {
		if out, err := exec.Command("go", "build", "-o", target, pkg).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, out)
		}
	}
	cfg := filepath.Join(tmp, "cfg.yaml")
	yaml := "station:\n  logical_servers:\n    - id: slow\n      name: Slow\n      transport: process\n      process: {command: " + fake + ", args: []}\n      serve: {http: {path: /slow}}\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(home string, stdin string, args ...string) (string, error) {
		c := exec.Command(bin, args...)
		c.Dir = tmp
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_DATA_HOME=" + home + "/data",
			"XDG_STATE_HOME=" + home + "/state", "XDG_CONFIG_HOME=" + home + "/config", "SUBSTRATE_CONFIG=" + cfg}
		c.Stdin = strings.NewReader(stdin)
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
	for _, args := range [][]string{{"validate"}, {"list"}, {"--help"}, {"help"}, {"secret", "check", "file://ns/missing"}} {
		home := t.TempDir()
		out, err := run(home, "", args...)
		if args[0] != "secret" && err != nil {
			t.Errorf("%v failed: %v\n%s", args, err, out)
		}
		if got := entries(home); len(got) != 0 {
			t.Errorf("%v created %v", args, got)
		}
	}
	// Lines cobra rejects must be rejected before anything is created: a bare `station` prints
	// help, and a bad flag, an extra argument or a missing required flag starts nothing.
	for _, args := range [][]string{{"--bogus"}, {"serve", "--port", "1"}, {"serve", "extra"}, {"identity", "setup"},
		{"identity", "key", "create", "--name", "n"}, {"secret", "set"}} {
		home := t.TempDir()
		if out, err := run(home, "", args...); err == nil {
			t.Errorf("%v succeeded, want a failure\n%s", args, out)
		}
		if got := entries(home); len(got) != 0 {
			t.Errorf("%v created %v", args, got)
		}
	}
	for _, args := range [][]string{{"identity", "setup", "--help"}, {"identity", "key", "create", "--help"}} {
		home := t.TempDir()
		out, err := run(home, "", args...)
		if err != nil || !strings.Contains(out, "--username") && !strings.Contains(out, "--scope") {
			t.Errorf("%v: err=%v\n%s", args, err, out)
		}
		if got := entries(home); len(got) != 0 {
			t.Errorf("%v created %v", args, got)
		}
	}
	for _, args := range [][]string{nil, {"serve", "--help=true"}} {
		home := t.TempDir()
		if out, err := run(home, "", args...); err != nil {
			t.Errorf("%v failed: %v\n%s", args, err, out)
		}
		if got := entries(home); len(got) != 0 {
			t.Errorf("%v created %v", args, got)
		}
	}
	// serve on a data directory with no database is refused and creates nothing.
	home := t.TempDir()
	if out, err := run(home, "", "serve"); err == nil || !strings.Contains(out, "no database") {
		t.Errorf("serve with no database: err=%v\n%s", err, out)
	}
	if got := entries(home); len(got) != 0 {
		t.Errorf("serve created %v", got)
	}
	home = t.TempDir()
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
	t.Setenv("STATION_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("SUBSTRATE_CONFIG", "")
	modules, mode := plan([]string{"validate"})
	run, err := boot.StartWith(context.Background(), substrateApp, modules, substrateConfigJSON, appboot.Options(os.Getenv, modules, mode))
	if err != nil {
		t.Fatal(err)
	}
	defer run.Stop(context.Background()) //nolint:errcheck
	root := (&station{app: run.App}).root(context.Background(), nil)
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
