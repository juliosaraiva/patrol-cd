package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/spf13/cobra"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
)

// NewGPGCommand returns `patrol gpg` (API-075, REPO-066).
func NewGPGCommand(f *cmdutil.Factory) *cobra.Command {
	c := &cobra.Command{
		Use:   "gpg",
		Short: "Manage GPG keys used for signature verification",
		RunE:  func(c *cobra.Command, args []string) error { return cmdutil.HelpExit(c, args) },
	}
	c.AddCommand(newGPGListCommand(f), newGPGGetCommand(f), newGPGAddCommand(f), newGPGDeleteCommand(f))
	return c
}

func newGPGListCommand(f *cmdutil.Factory) *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:   "list",
		Short: "List configured GPG public keys",
		Example: `  # List all configured GPG public keys in wide format (default).
  patrol gpg list

  # List all configured GPG public keys in JSON format.
  patrol gpg list -o json

  # List all configured GPG public keys in YAML format.
  patrol gpg list -o yaml`,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := output.CheckFormat(format, output.JSON, output.YAML, output.Wide, ""); err != nil {
				return output.UnknownFormat(format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			keys, err := client.ListGPGKeys(c.Context())
			if err != nil {
				return err
			}
			if format == output.JSON || format == output.YAML {
				return output.PrintResourceList(f.Out(), keys.Items, format, false)
			}
			t := output.NewTable(f.Out())
			t.Row("KEYID", "TYPE", "IDENTITY")
			for _, k := range keys.Items {
				t.Row(k.KeyID, strings.ToUpper(k.SubType), k.Owner)
			}
			return t.Flush()
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. One of: json|yaml|wide")
	return c
}

func newGPGGetCommand(f *cmdutil.Factory) *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:   "get KEYID",
		Short: "Get the GPG public key with ID <KEYID> from the server",
		Example: `  # Get a GPG public key with the specified KEYID in wide format (default).
  patrol gpg get KEYID

  # Get a GPG public key with the specified KEYID in JSON format.
  patrol gpg get KEYID -o json

  # Get a GPG public key with the specified KEYID in YAML format.
  patrol gpg get KEYID -o yaml`,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 1 {
				return errors.New("Missing KEYID argument") //nolint:staticcheck // upstream wording
			}
			if err := output.CheckFormat(format, output.JSON, output.YAML, output.Wide, ""); err != nil {
				return output.UnknownFormat(format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			key, err := client.GetGPGKey(c.Context(), args[0])
			if err != nil {
				return err
			}
			if format == output.JSON || format == output.YAML {
				return output.PrintResource(f.Out(), key, format)
			}
			w := f.Out()
			output.Printf(w, "Key ID:          %s\n", key.KeyID)
			output.Printf(w, "Key fingerprint: %s\n", key.Fingerprint)
			output.Printf(w, "Key subtype:     %s\n", strings.ToUpper(key.SubType))
			output.Printf(w, "Key owner:       %s\n", key.Owner)
			_, _ = fmt.Fprintf(w, "Key data follows until EOF:\n%s\n", output.SanitizeMultiline(key.KeyData))
			return nil
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. One of: json|yaml|wide")
	return c
}

func newGPGAddCommand(f *cmdutil.Factory) *cobra.Command {
	var fromFile string
	c := &cobra.Command{
		Use:   "add",
		Short: "Adds a GPG public key to the server's keyring",
		Example: `  # Add a GPG public key to the server's keyring from a file.
  patrol gpg add --from /path/to/keyfile`,
		Args: cmdutil.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if fromFile == "" {
				return errors.New("--from is mandatory")
			}
			data, err := readInputFile(f, fromFile)
			if err != nil {
				return err
			}
			keyData, err := armorPublicKeys(data)
			if err != nil {
				return err
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			res, err := client.CreateGPGKeys(c.Context(), keyData)
			if err != nil {
				return err
			}
			msg := fmt.Sprintf("Created %d key(s) from input file", len(res.Created.Items))
			if len(res.Skipped) > 0 {
				msg += fmt.Sprintf(", and %d key(s) were skipped because they exist already", len(res.Skipped))
			}
			output.Println(f.Out(), msg+".")
			return nil
		},
	}
	c.Flags().StringVarP(&fromFile, "from", "f", "", "Path to the file that contains the GPG public key to import")
	return c
}

// armorPublicKeys returns ASCII-armored key data: armored input is passed through,
// binary key material is armored (the API accepts armored keys).
func armorPublicKeys(data []byte) (string, error) {
	if bytes.Contains(data, []byte("-----BEGIN PGP")) {
		return string(data), nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return "", errors.New("the key file is empty")
	}
	var b bytes.Buffer
	w, err := armor.Encode(&b, "PGP PUBLIC KEY BLOCK", nil)
	if err != nil {
		return "", fmt.Errorf("armor key data: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return "", fmt.Errorf("armor key data: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("armor key data: %w", err)
	}
	return b.String(), nil
}

func newGPGDeleteCommand(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "rm KEYID",
		Short: "Removes a GPG public key from the server's keyring",
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 1 {
				return errors.New("Missing KEYID argument") //nolint:staticcheck // upstream wording
			}
			id := args[0]
			ok, err := f.OptionalPrompt().Confirm(fmt.Sprintf("Are you sure you want to remove '%s'? [y/n] ", id))
			if err != nil {
				return err
			}
			if !ok {
				output.Printf(f.Out(), "The command to delete key with key ID '%s' was cancelled.\n", id)
				return nil
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			if err := client.DeleteGPGKey(c.Context(), id); err != nil {
				return err
			}
			output.Printf(f.Out(), "Deleted key with key ID %s\n", id)
			return nil
		},
	}
}
