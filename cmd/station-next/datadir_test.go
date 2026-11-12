package main

import (
	"context"
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
	for _, tc := range []struct {
		args    []string
		modules []string
		prepare bool
	}{
		{nil, substrateModules, true},
		{[]string{"serve"}, substrateModules, true},
		{[]string{"serve", "--help"}, readOnly, false},
		{[]string{"validate"}, readOnly, false},
		{[]string{"list"}, readOnly, false},
		{[]string{"--help"}, readOnly, false},
		{[]string{"help"}, readOnly, false},
		{[]string{"completion", "bash"}, readOnly, false},
		{[]string{"identity", "setup", "--username", "owner"}, withIdentity, true},
		{[]string{"identity", "key", "create", "--name", "n", "--scope", "s", "--stdout"}, withIdentity, true},
		{[]string{"identity"}, readOnly, false},
		{[]string{"identity", "key"}, readOnly, false},
		{[]string{"secret", "check", "file://a/b"}, withSecrets, false},
		{[]string{"secret", "set", "file://a/b"}, withSecrets, true},
		{[]string{"nonsense"}, readOnly, false},
	} {
		mods, prepare := plan(tc.args)
		if !slices.Equal(mods, pickOrAll(tc.modules)) || prepare != tc.prepare {
			t.Errorf("plan(%v) = %v, %v; want %v, %v", tc.args, mods, prepare, tc.modules, tc.prepare)
		}
	}
}

// pickOrAll orders a module set the way plan does.
func pickOrAll(want []string) []string {
	if slices.Equal(want, substrateModules) {
		return want
	}
	return pick(want)
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
	t.Setenv("STATION_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("SUBSTRATE_CONFIG", "")
	modules, prepare := plan([]string{"validate"})
	run, err := boot.StartWith(context.Background(), substrateApp, modules, substrateConfigJSON, boot.Options{AfterConfig: afterConfig(os.Getenv, prepare)})
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
