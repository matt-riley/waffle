// Package backup provides local, filesystem backups of waffle state.
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"filippo.io/age"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/store"
)

type manifest struct {
	Version  string `json:"version"`
	Identity bool   `json:"identity_included"`
}

// Create writes a new, never-overwritten directory backup.
func Create(ctx context.Context, dst string, withIdentity bool, identity string) error {
	if filepath.IsAbs(dst) == false {
		return errors.New("backup destination must be an absolute path")
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("backup destination already exists: %s", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	cleanup := func() { _ = os.RemoveAll(dst) }
	defer func() {
		if _, err := os.Stat(filepath.Join(dst, "manifest.json")); err != nil {
			cleanup()
		}
	}()

	db, err := config.DBPath()
	if err != nil {
		return err
	}
	if ok, err := store.Snapshot(ctx, db, filepath.Join(dst, "waffle.db")); err != nil {
		return err
	} else if !ok {
		// An empty state is represented by no database file.
		_ = os.Remove(filepath.Join(dst, "waffle.db"))
	}
	home, err := config.Home()
	if err != nil {
		return err
	}
	for _, name := range []string{"secrets.age", "config.toml"} {
		if err := copyIfExists(filepath.Join(home, name), filepath.Join(dst, name), 0o600); err != nil {
			return err
		}
	}
	for _, name := range []string{"workspace", "skills"} {
		if err := copyDirIfExists(filepath.Join(home, name), filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	if withIdentity {
		if identity == "" {
			return errors.New("identity is required with --with-identity")
		}
		if err := os.WriteFile(filepath.Join(dst, "identity"), []byte(identity+"\n"), 0o600); err != nil {
			return err
		}
	}
	b, _ := json.MarshalIndent(manifest{Version: "dev", Identity: withIdentity}, "", "  ")
	if err := os.WriteFile(filepath.Join(dst, "manifest.json"), append(b, '\n'), 0o600); err != nil {
		return err
	}
	return nil
}

// Restore validates the backup database and configuration before replacing
// any live files. The backup directory itself is never modified.
func Restore(ctx context.Context, src string) error {
	if !filepath.IsAbs(src) {
		return errors.New("restore source must be an absolute path")
	}
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return fmt.Errorf("invalid backup directory %q", src)
	}
	if _, err := os.Stat(filepath.Join(src, "manifest.json")); err != nil {
		return errors.New("backup manifest.json is missing")
	}
	if p := filepath.Join(src, "config.toml"); exists(p) {
		if _, err := config.Load(p); err != nil {
			return fmt.Errorf("validate config: %w", err)
		}
	}
	// Validate ciphertext before touching live state whenever an identity is
	// available. A bundled identity is parsed but never installed implicitly.
	if exists(filepath.Join(src, "secrets.age")) {
		var id *age.X25519Identity
		if b, e := os.ReadFile(filepath.Join(src, "identity")); e == nil {
			id, e = age.ParseX25519Identity(string(b))
			if e != nil {
				return fmt.Errorf("validate bundled identity: %w", e)
			}
		} else if current, e := secret.LoadIdentity(); e == nil {
			id = current
		}
		if id != nil {
			raw, e := os.ReadFile(filepath.Join(src, "secrets.age"))
			if e != nil {
				return e
			}
			r, e := age.Decrypt(bytes.NewReader(raw), id)
			if e != nil {
				return fmt.Errorf("validate secret store: %w", e)
			}
			if _, e = io.ReadAll(r); e != nil {
				return fmt.Errorf("validate secret store: %w", e)
			}
		}
	}
	home, err := config.Home()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	stage, err := os.MkdirTemp(home, ".restore-*")
	if err != nil {
		return fmt.Errorf("create restore staging area: %w", err)
	}
	defer os.RemoveAll(stage)
	if p := filepath.Join(src, "waffle.db"); exists(p) {
		cp := filepath.Join(stage, "waffle.db")
		if err := copyFile(p, cp, 0o600); err != nil {
			return err
		}
		st, err := store.Open(ctx, cp)
		if err != nil {
			return fmt.Errorf("validate database snapshot: %w", err)
		}
		_ = st.Close()
	}
	// Stage every file before touching live state.
	for _, name := range []string{"config.toml", "secrets.age", "identity"} {
		if exists(filepath.Join(src, name)) {
			if err := copyFile(filepath.Join(src, name), filepath.Join(stage, name), 0o600); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"workspace", "skills"} {
		if err := copyDirIfExists(filepath.Join(src, name), filepath.Join(stage, name)); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"waffle.db", "config.toml", "secrets.age"} {
		if exists(filepath.Join(stage, name)) {
			if err := replaceFile(filepath.Join(stage, name), filepath.Join(home, name)); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"workspace", "skills"} {
		if exists(filepath.Join(stage, name)) {
			if err := replaceDir(filepath.Join(stage, name), filepath.Join(home, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func copyIfExists(src, dst string, mode fs.FileMode) error {
	if !exists(src) {
		return nil
	}
	return copyFile(src, dst, mode)
}
func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
func copyDirIfExists(src, dst string) error {
	if !exists(src) {
		return nil
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		return copyFile(path, target, 0o600)
	})
}
func replaceFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.Rename(src, dst)
}
func replaceDir(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.Rename(src, dst)
}
