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
	"github.com/matt-riley/waffle/internal/filecommit"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/store"
)

type manifest struct {
	Version  string `json:"version"`
	Identity bool   `json:"identity_included"`
}

// Create writes a new, never-overwritten directory backup.
func Create(ctx context.Context, dst string, withIdentity bool, identity string) (retErr error) {
	if !filepath.IsAbs(dst) {
		return errors.New("backup destination must be an absolute path")
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("backup destination already exists: %s", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Recorded before the tree exists so the entries MkdirAll creates can be
	// made durable at the end: fsyncing the files inside a directory whose own
	// entry never reached stable storage loses the whole backup (#263).
	created := missingDirs(dst)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	cleanup := func() { _ = os.RemoveAll(dst) }
	defer func() {
		// Keyed on the outcome, not only on the marker: a failure after the
		// manifest lands must not leave a destination that Restore accepts as
		// complete and that a retry cannot overwrite. The marker check stays
		// for the panic path, where retErr is never set.
		if retErr != nil {
			cleanup()
			return
		}
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
		nested, err := copyDirIfExists(filepath.Join(home, name), filepath.Join(dst, name))
		if err != nil {
			return err
		}
		// Deepest first, ahead of the destination chain, so every entry is
		// synced before the directory that holds it.
		created = append(nested, created...)
	}
	if withIdentity {
		if identity == "" {
			return errors.New("identity is required with --with-identity")
		}
		if err := filecommit.Write(filepath.Join(dst, "identity"), []byte(identity+"\n"), 0o600); err != nil {
			return err
		}
	}
	// Every directory this backup created is made durable before the marker is
	// written, so a recovered filesystem never shows a complete backup whose
	// content is missing.
	if err := syncCreatedDirs(created); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(manifest{Version: "dev", Identity: withIdentity}, "", "  ")
	// manifest.json is this backup's completion marker, so it is committed
	// durably (filecommit syncs the file and its directory) and last (#263).
	return filecommit.Write(filepath.Join(dst, "manifest.json"), append(b, '\n'), 0o600)
}

// missingDirs returns the directories MkdirAll would have to create for path,
// deepest first. Everything above the first existing ancestor is already
// durable.
func missingDirs(path string) []string {
	var missing []string
	for current := path; ; {
		if _, err := os.Stat(current); err == nil {
			break
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return missing
}

// syncCreatedDirs makes each newly created directory entry durable by syncing
// the directory that holds it.
func syncCreatedDirs(created []string) error {
	for _, dir := range created {
		if err := filecommit.SyncDir(filepath.Dir(dir)); err != nil {
			return err
		}
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
	// Best-effort: staged files are under home and cleaned up after rename
	// or on the next restore attempt if removal fails.
	defer func() { _ = os.RemoveAll(stage) }()
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
		// Restore stages into a temp dir under home and renames it into place,
		// so the created entries here are not the durable ones.
		if _, err := copyDirIfExists(filepath.Join(src, name), filepath.Join(stage, name)); err != nil {
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
func copyFile(src, dst string, mode fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := in.Close(); err == nil {
			err = cerr
		}
	}()
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
	// A backup that reported success used to be free to lose its non-database
	// files on power loss: Close only hands the bytes to the page cache (#263).
	// Streamed rather than staged because this also copies waffle.db, which is
	// as large as the owner's history.
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return filecommit.SyncDir(filepath.Dir(dst))
}

// copyDirIfExists mirrors src into dst and reports the directories it created,
// deepest first. copyFile syncs the directory each file lands in, but a
// directory holding only subdirectories is never a file's parent, so its own
// entry needs syncing by the caller or the subtree can vanish under a backup
// that reported success (#263).
func copyDirIfExists(src, dst string) ([]string, error) {
	if !exists(src) {
		return nil, nil
	}
	var created []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			// WalkDir is parent-first, so prepending yields deepest-first.
			created = append(missingDirs(target), created...)
			return os.MkdirAll(target, 0o700)
		}
		return copyFile(path, target, 0o600)
	})
	if err != nil {
		return nil, err
	}
	return created, nil
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
