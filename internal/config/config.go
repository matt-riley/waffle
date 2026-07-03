// Package config loads waffle's configuration from $WAFFLE_HOME/config.toml.
//
// The config file is deliberately small (docs/plan.md, "minimal config"):
// it names the trust boundaries — listen address, agent-group policies,
// secret references — and nothing else. Behavior belongs in code, secrets
// belong in the secret store, and the file is optional: waffle runs with
// defaults until one exists.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the root of config.toml.
type Config struct {
	Gateway Gateway `toml:"gateway"`
	Log     Log     `toml:"log"`
}

// Gateway configures the control plane.
type Gateway struct {
	// Listen is the bind address. Loopback by default: exposing the
	// gateway remotely is a deliberate, explicit decision.
	Listen string `toml:"listen"`
}

// Log configures logging.
type Log struct {
	Level string `toml:"level"`
}

// Default returns the configuration waffle runs with when no file exists.
func Default() Config {
	return Config{
		Gateway: Gateway{Listen: "127.0.0.1:8420"},
		Log:     Log{Level: "info"},
	}
}

// Home returns waffle's state directory: $WAFFLE_HOME if set, else ~/.waffle.
func Home() (string, error) {
	if h := os.Getenv("WAFFLE_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".waffle"), nil
}

func homePath(name string) (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, name), nil
}

// Path returns the location of config.toml.
func Path() (string, error) { return homePath("config.toml") }

// DBPath returns the location of waffle's SQLite database.
func DBPath() (string, error) { return homePath("waffle.db") }

// SecretsPath returns the location of the encrypted secret store.
func SecretsPath() (string, error) { return homePath("secrets.age") }

// Load reads the config file at path, layered over Default. A missing file
// is not an error. Unknown keys are: they are almost always typos, and a
// silently ignored policy key is a security bug.
func Load(path string) (Config, error) {
	cfg := Default()
	meta, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("unknown keys in %s: %s", path, strings.Join(keys, ", "))
	}
	return cfg, nil
}
