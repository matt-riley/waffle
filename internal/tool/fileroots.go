package tool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutsideRoots is returned when a file tool is handed a path outside its
// configured roots.
var ErrOutsideRoots = errors.New("path is outside the allowed roots")

// FileRoots confines the builtin file tools (read_file, write_file, edit_file,
// search) to a set of directory trees (#269).
//
// Tool policy can deny a tool *name*, but nothing stopped a host-mode session
// from reading ~/.ssh/id_ed25519 or writing /etc — docker mode gets OS-level
// isolation, host mode had none. FileRoots is that missing boundary; it is
// defence in depth, not a substitute for the sandbox.
//
// The zero value allows every path, which is what an unconfigured owner
// deployment gets. Once roots are set they are absolute and symlink-resolved,
// and Resolve refuses anything that escapes them — including via a symlink
// planted inside a root, which is checked against the resolved target rather
// than the name the model supplied.
type FileRoots struct {
	roots []string
}

// NewFileRoots builds a boundary from directory paths. Paths are made
// absolute and symlink-resolved once, at construction: an agent cannot make a
// root move afterwards, and per-call work stays to one EvalSymlinks. A root
// that does not exist is an error — a typo would otherwise silently confine a
// session to nothing, or (worse) read as "no boundary".
func NewFileRoots(paths []string) (FileRoots, error) {
	var fr FileRoots
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			return FileRoots{}, errors.New("file root must not be empty")
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return FileRoots{}, fmt.Errorf("file root %q: %w", p, err)
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return FileRoots{}, fmt.Errorf("file root %q: %w", p, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return FileRoots{}, fmt.Errorf("file root %q: %w", p, err)
		}
		if !info.IsDir() {
			return FileRoots{}, fmt.Errorf("file root %q is not a directory", p)
		}
		fr.roots = append(fr.roots, filepath.Clean(resolved))
	}
	return fr, nil
}

// Unrestricted reports whether no boundary is configured.
func (fr FileRoots) Unrestricted() bool { return len(fr.roots) == 0 }

// Roots returns the resolved root directories.
func (fr FileRoots) Roots() []string { return append([]string(nil), fr.roots...) }

// Confines reports whether every root of other is inside a root of fr. It is
// how a profile is held to "may only tighten": a narrower posture may subdivide
// the group's roots, never reach outside them.
//
// Named for the direction it reads at the call site — groupRoots.Confines(
// profileRoots) — because a reversed receiver and argument would silently
// admit exactly the boundary this is here to refuse.
func (fr FileRoots) Confines(other FileRoots) bool {
	if fr.Unrestricted() {
		return true // no boundary to widen past
	}
	if other.Unrestricted() {
		return false
	}
	for _, r := range other.roots {
		if !fr.contains(r) {
			return false
		}
	}
	return true
}

// Resolve validates path against the boundary and returns the path the tool
// should actually use. The returned path has its existing ancestors
// symlink-resolved, so a caller that opens it cannot be redirected by a link
// that was checked but not followed.
func (fr FileRoots) Resolve(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if fr.Unrestricted() {
		return abs, nil
	}
	resolved, err := resolveExisting(abs)
	if err != nil {
		return "", err
	}
	if !fr.contains(resolved) {
		return "", fmt.Errorf("%w: %s (allowed: %s)", ErrOutsideRoots, path, strings.Join(fr.roots, ", "))
	}
	return resolved, nil
}

func (fr FileRoots) contains(path string) bool {
	for _, root := range fr.roots {
		if path == root {
			return true
		}
		if strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// resolveExisting symlink-resolves the longest existing prefix of path and
// re-appends the rest. filepath.EvalSymlinks fails outright on a path that
// does not exist yet, but write_file legitimately creates new files (and
// parent directories), so the check has to apply to the deepest ancestor that
// does exist — that is the part an attacker could have made point elsewhere.
func resolveExisting(path string) (string, error) {
	remainder := ""
	current := filepath.Clean(path)
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			if remainder == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, remainder), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the root without finding anything that exists.
			return filepath.Clean(path), nil
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}
