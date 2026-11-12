// Package appcmd holds the commands every Station binary has.
package appcmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/substrate/module"
	"github.com/hollis-labs/substrate/modules/secrets"
)

// Secret provisions and checks the credential references a logical server's env refers to
// (for example file://design/provider-key). The value is read from stdin, never from argv, so no
// key is in a shell history, a process listing or a config file; check prints where a reference
// resolves from and how long the value is, never the value.
func Secret(app *module.App) *cobra.Command {
	root := &cobra.Command{Use: "secret", Short: "Provision and check credential references"}
	root.AddCommand(&cobra.Command{
		Use:   "set <ref>",
		Short: "Store the value read from stdin under a reference",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			r := secrets.Resolver(app)
			if r == nil {
				return errors.New("the secrets module must be started")
			}
			raw, err := io.ReadAll(io.LimitReader(c.InOrStdin(), 64<<10))
			if err != nil {
				return errors.New("secret set: read stdin failed")
			}
			value := strings.TrimRight(string(raw), "\r\n")
			if value == "" {
				return errors.New("secret set: empty value on stdin")
			}
			if err := r.Set(c.Context(), args[0], strings.NewReader(value)); err != nil {
				return fmt.Errorf("secret set %s: %w", args[0], err)
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "%s: stored %s\n", app.Name, args[0])
			return err
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "check <ref>",
		Short: "Resolve a reference and say where it came from, never the value",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			r := secrets.Resolver(app)
			if r == nil {
				return errors.New("the secrets module must be started")
			}
			res, err := r.Resolve(c.Context(), args[0])
			if err != nil {
				return fmt.Errorf("secret check %s: %w", args[0], err)
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "%s: %s resolves (source %s, %d bytes)\n", app.Name, args[0], res.Source, res.Secret.Len())
			return err
		},
	})
	return root
}
