package plugin

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// ErrPathOutsideRoot rejects a package-supplied path that does not resolve
// within the filesystem-resolved plugin root (spec §4.1).
var ErrPathOutsideRoot = errors.New("plugin path escapes the plugin root")

// ResolveWithin resolves a plugin-supplied relative path against root and
// returns the filesystem-resolved target, enforcing spec §4.1: rel must
// begin with "./", is resolved against the plugin root, and must remain
// within the filesystem-resolved root after symlink resolution. Symlinks
// may resolve anywhere inside the root; anything resolving outside it is
// rejected, and a target that cannot be resolved is rejected too — an
// unresolvable path cannot be proven contained.
//
// This is the general containment rule shared by every component loader
// (skills/, mcp.json command/cwd). It is deliberately distinct from
// internal/skillinstall's staging rule, which rejects symlinks outright in
// untrusted fetched trees.
func ResolveWithin(root, rel string) (string, error) {
	if rel == "" || !strings.HasPrefix(rel, "./") {
		return "", fmt.Errorf(`%w: %q must begin with "./"`, ErrPathOutsideRoot, rel)
	}
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: %q contains a NUL byte", ErrPathOutsideRoot, rel)
	}
	cleaned := path.Clean(filepath.ToSlash(rel))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return "", fmt.Errorf("%w: %q escapes the plugin root", ErrPathOutsideRoot, rel)
	}

	resolvedRoot, err := resolvedDir(root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(resolvedRoot, filepath.FromSlash(cleaned))
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve plugin path %q: %w", rel, err)
	}
	if !withinDir(resolvedRoot, resolvedTarget) {
		return "", fmt.Errorf("%w: %q resolves outside the plugin root", ErrPathOutsideRoot, rel)
	}
	return resolvedTarget, nil
}

// withinDir reports whether target is root or a descendant of it. Both
// arguments must already be filesystem-resolved (no symlink components).
func withinDir(root, target string) bool {
	if target == root {
		return true
	}
	prefix := strings.TrimRight(root, string(filepath.Separator)) + string(filepath.Separator)
	return strings.HasPrefix(target, prefix)
}

// PluginsDir returns waffle's installed-plugins root under the waffle state
// directory home (config.Home: $WAFFLE_HOME or ~/.waffle): <home>/plugins.
// Plugins are placed on disk by the owner; installation, download, and
// registry mechanics are deliberately out of scope (#389).
func PluginsDir(home string) string {
	return filepath.Join(home, "plugins")
}

// InstallRoot returns the conventional installed location of the named
// plugin: <home>/plugins/<name>. Plugin-supplied skills are canonical
// under this root; the agent-curated <workspace>/skills tier stays
// separate. PLUGIN_DATA (spec §9) gains an obvious sibling under
// <home>/plugins in a later issue.
func InstallRoot(home, name string) (string, error) {
	if home == "" {
		return "", errors.New("plugin install root requires the waffle home directory")
	}
	if !ValidName(name) {
		return "", rejectName(name)
	}
	return filepath.Join(PluginsDir(home), name), nil
}
