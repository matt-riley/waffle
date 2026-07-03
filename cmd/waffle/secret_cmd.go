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

Names look like "anthropic/api-key". Reference them from config.toml as
"secret://anthropic/api-key" — config never holds raw values.
`)
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
