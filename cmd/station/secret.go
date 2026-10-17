package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hollis-labs/libs/security/secrets"
)

// runSecret provisions and checks secret references (provider API keys).
//
//	station secret set <ref>     value read from stdin, never from argv
//	station secret check <ref>   resolves the ref; prints where it came from, never the value
//
// A ref such as keychain://openai/work names a secret held in the OS
// keychain; api.yaml then says `api_key_ref: keychain://openai/work`, so no key
// is ever in a config file, an environment dump or a shell history.
func runSecret(args []string) int {
	fs := flag.NewFlagSet("station secret", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil || fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: station secret set|check <ref>   (set reads the value from stdin)")
		return 2
	}
	verb, ref := fs.Arg(0), fs.Arg(1)
	ctx := context.Background()
	r := secrets.New(secrets.WithLegacy("tether", func(ref secrets.Ref) string {
		if ref.Authority == "" || len(ref.Path) == 0 {
			return ""
		}
		return "provider-api-key:" + ref.Key()
	}))
	switch verb {
	case "set":
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
		if err != nil {
			fmt.Fprintln(os.Stderr, "station: secret set: read stdin failed")
			return 1
		}
		value := strings.TrimRight(string(raw), "\r\n")
		if value == "" {
			fmt.Fprintln(os.Stderr, "station: secret set: empty value on stdin")
			return 1
		}
		if err := r.Set(ctx, ref, strings.NewReader(value)); err != nil {
			fmt.Fprintf(os.Stderr, "station: secret set %s: %v\n", ref, err)
			return 1
		}
		fmt.Printf("station: stored %s\n", ref)
		return 0
	case "check":
		res, err := r.Resolve(ctx, ref)
		if err != nil {
			fmt.Fprintf(os.Stderr, "station: secret check %s: %v\n", ref, err)
			return 1
		}
		fmt.Printf("station: %s resolves (source %s, %d bytes)\n", ref, res.Source, res.Secret.Len())
		return 0
	}
	fmt.Fprintln(os.Stderr, "usage: station secret set|check <ref>")
	return 2
}
