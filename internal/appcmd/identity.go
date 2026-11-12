package appcmd

import (
	"errors"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/substrate/module"
	"github.com/hollis-labs/substrate/modules/identity"
)

// IdentityHelp returns a help-only copy of the offline identity commands, or nil when the identity
// module started and the real ones are registered. The identity commands exist only when that module
// started, which a bare `identity setup --help` never does (help starts nothing that opens a
// file), so without these the operator would be told the command is unknown. The copies carry
// the same flags; they never run: a line that reaches one was not recognised as a complete command
// line, and the error says so.
func IdentityHelp(app *module.App) *cobra.Command {
	if identity.From(app) != nil {
		return nil
	}
	stub := func(use, short, long string) *cobra.Command {
		return &cobra.Command{
			Use: use, Short: short, Long: long, Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				return errors.New("nothing was run: give every required flag (see --help); this command line did not start the identity module")
			},
		}
	}
	setup := stub("setup", "Create the first owner in the database (no server; the password is read from stdin or a prompt)",
		"Creates the first owner with every scope. The password is read from the terminal (prompted twice, not echoed)\n"+
			"or, when stdin is not a terminal, from its first line. It is never an argument or an environment variable.\n"+
			"Refuses when a user already exists: first-run setup closes for good.")
	setup.Flags().String("username", "", "owner's username (required)")
	setup.Flags().String("display-name", "", "owner's display name (default: the username)")

	create := stub("create", "Create an API key without a server",
		"Creates an API key for a named principal. The key is shown once: it is written to the file named by --out\n"+
			"(new file, mode 0600, never overwritten) or, with --stdout, to stdout as the only output. Any scope may be\n"+
			"granted. Needs an owner: run setup first.")
	create.Flags().String("name", "", "principal name (required)")
	create.Flags().StringSlice("scope", nil, "scope to grant; repeat or comma-separate (required; any scope, including *)")
	create.Flags().StringSlice("audience", nil, "audience the key is limited to; repeatable")
	create.Flags().Duration("ttl", time.Duration(0), "key lifetime, for example 720h (default: no expiry)")
	create.Flags().String("out", "", "write the key to this new file (mode 0600)")
	create.Flags().Bool("stdout", false, "write the key to stdout, and nothing else")
	key := &cobra.Command{Use: "key", Short: "API keys"}
	key.AddCommand(create)

	root := &cobra.Command{Use: "identity", Short: "Offline identity bootstrap (owner and API keys, no server)"}
	root.AddCommand(setup, key)
	return root
}
