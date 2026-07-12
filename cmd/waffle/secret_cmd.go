package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/secret"
)

func secretCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		secretUsage(stderr)
		return errUsage
	}
	switch args[0] {
	case "init":
		printOnly := len(args) > 1 && args[1] == "--print"
		return secretInit(printOnly, stdout)
	case "set":
		if len(args) != 2 {
			return errors.New("usage: waffle secret set <name>")
		}
		return secretSet(args[1], stdin, stderr)
	case "get":
		if len(args) != 2 {
			return errors.New("usage: waffle secret get <name>")
		}
		return secretGet(args[1], stdout)
	case "ls":
		return secretList(stdout)
	case "rm":
		if len(args) != 2 {
			return errors.New("usage: waffle secret rm <name>")
		}
		return secretDelete(args[1])
	case "export-identity":
		return secretExportIdentity(args[1:], stdin, stdout, stderr)
	case "import-identity":
		return secretImportIdentity(args[1:], stdin, stdout, stderr)
	case "help", "-h", "--help":
		secretUsage(stdout)
		return nil
	default:
		secretUsage(stderr)
		return fmt.Errorf("unknown secret command %q", args[0])
	}
}

func secretUsage(w io.Writer) {
	fmt.Fprint(w, `Manage the encrypted secret store (~/.waffle/secrets.age).

Usage:
  waffle secret init [--print]   create the store identity (keyring, or print it)
  waffle secret set <name>       store a secret (value read from stdin, no echo)
  waffle secret get <name>       print a secret value
  waffle secret ls               list secret names
  waffle secret rm <name>        delete a secret
  waffle secret export-identity [--yes] [--output <file>]
                                export the keyring identity (0600 file)
  waffle secret import-identity [--yes] [--file <file>]
                                import an identity into the keyring

Names look like "anthropic/api-key". Reference them from config.toml as
"secret://anthropic/api-key" — config never holds raw values.
`)
}

func confirm(stdin io.Reader, stderr io.Writer, yes bool, prompt string) error {
	if yes {
		return nil
	}
	fmt.Fprintf(stderr, "%s [y/N] ", prompt)
	var answer string
	if _, err := fmt.Fscan(stdin, &answer); err != nil {
		return errors.New("confirmation required (use --yes for noninteractive use)")
	}
	if strings.ToLower(answer) != "y" && strings.ToLower(answer) != "yes" {
		return errors.New("cancelled")
	}
	return nil
}

func secretExportIdentity(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	yes := false
	output := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--yes":
			yes = true
		case "--output":
			if i+1 >= len(args) {
				return errors.New("--output requires a path")
			}
			output = args[i+1]
			i++
		default:
			return fmt.Errorf("unknown export-identity option %q", args[i])
		}
	}
	if err := confirm(stdin, stderr, yes, "Export the secret-store identity? Anyone with it can decrypt your secrets."); err != nil {
		return err
	}
	id, err := secret.LoadIdentity()
	if err != nil {
		return err
	}
	if output == "" {
		_, err = fmt.Fprintln(stdout, id)
		return err
	}
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(id.String() + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(output)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "identity exported to %s (keep it private)\n", output)
	return nil
}

func secretImportIdentity(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	yes := false
	file := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--yes":
			yes = true
		case "--file":
			if i+1 >= len(args) {
				return errors.New("--file requires a path")
			}
			file = args[i+1]
			i++
		default:
			return fmt.Errorf("unknown import-identity option %q", args[i])
		}
	}
	if err := confirm(stdin, stderr, yes, "Import and save this secret-store identity to the OS keyring?"); err != nil {
		return err
	}
	var b []byte
	var err error
	if file != "" {
		b, err = os.ReadFile(file)
	} else {
		b, err = io.ReadAll(stdin)
	}
	if err != nil {
		return err
	}
	if err := secret.ImportIdentity(string(b)); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "secret-store identity imported")
	return nil
}

func openSecretStore() (secret.Store, error) {
	id, err := secret.LoadIdentity()
	if err != nil {
		return nil, err
	}
	path, err := config.SecretsPath()
	if err != nil {
		return nil, err
	}
	return secret.OpenFile(path, id), nil
}

func secretInit(printOnly bool, stdout io.Writer) error {
	id, err := secret.InitIdentity(printOnly)
	if err != nil {
		return err
	}
	if printOnly {
		fmt.Fprintf(stdout, "%s\n", id)
		fmt.Fprintf(stdout, "# not stored anywhere — export it: %s=<identity>\n", secret.EnvIdentity)
		return nil
	}
	fmt.Fprintln(stdout, "secret-store identity created and saved to the OS keyring")
	fmt.Fprintf(stdout, "backup copy (store somewhere safe, then clear your scrollback):\n%s\n", id)
	return nil
}

func secretSet(name string, stdin io.Reader, stderr io.Writer) error {
	store, err := openSecretStore()
	if err != nil {
		return err
	}
	value, err := readSecretValue(stdin, stderr)
	if err != nil {
		return err
	}
	if value == "" {
		return errors.New("empty value")
	}
	return store.Set(name, value)
}

// readSecretValue reads without echo from a terminal, or the whole of stdin
// when piped (`waffle secret set foo < key.txt`).
func readSecretValue(stdin io.Reader, stderr io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, "value (input hidden): ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	b, err := io.ReadAll(stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func secretGet(name string, stdout io.Writer) error {
	store, err := openSecretStore()
	if err != nil {
		return err
	}
	v, err := store.Get(name)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, v)
	return nil
}

func secretList(stdout io.Writer) error {
	store, err := openSecretStore()
	if err != nil {
		return err
	}
	names, err := store.List()
	if err != nil {
		return err
	}
	for _, n := range names {
		fmt.Fprintln(stdout, n)
	}
	return nil
}

func secretDelete(name string) error {
	store, err := openSecretStore()
	if err != nil {
		return err
	}
	return store.Delete(name)
}
