//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

// Package localsocket owns Waffle's filesystem-authorized local listener.
package localsocket

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const (
	systemdFirstFD   = 3
	maxActivationFDs = 64
)

var activationEnvironment = [...]string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"}

// Listener consumes Waffle's systemd socket when one was passed, otherwise it
// creates the optional standalone Unix socket. The bool reports inheritance.
func Listener(configuredPath string) (net.Listener, bool, error) {
	if inherited, ok, err := inheritedListener("waffle-chat"); ok || err != nil {
		return inherited, ok, err
	}
	if configuredPath == "" {
		return nil, false, nil
	}
	listener, err := configuredListener(configuredPath)
	return listener, false, err
}

func inheritedListener(expectedName string) (net.Listener, bool, error) {
	pidValue, hasPID := os.LookupEnv("LISTEN_PID")
	fdsValue, hasFDs := os.LookupEnv("LISTEN_FDS")
	namesValue, hasNames := os.LookupEnv("LISTEN_FDNAMES")
	if !hasPID && !hasFDs && !hasNames {
		return nil, false, nil
	}
	if err := unsetActivationEnvironment(); err != nil {
		return nil, false, err
	}
	if !hasPID {
		return nil, false, errors.New("systemd activation is missing LISTEN_PID")
	}
	pid, err := strconv.Atoi(pidValue)
	if err != nil || pid <= 0 {
		return nil, false, fmt.Errorf("invalid systemd LISTEN_PID %q", pidValue)
	}
	if pid != os.Getpid() {
		return nil, false, nil
	}
	if !hasFDs {
		return nil, false, errors.New("systemd activation is missing LISTEN_FDS")
	}
	fds, err := strconv.Atoi(fdsValue)
	if err != nil || fds <= 0 {
		return nil, false, fmt.Errorf("invalid systemd LISTEN_FDS %q", fdsValue)
	}
	if fds > maxActivationFDs {
		return nil, false, fmt.Errorf("systemd LISTEN_FDS %d exceeds defensive limit %d", fds, maxActivationFDs)
	}
	if fds != 1 {
		validationErr := fmt.Errorf("systemd activation requires exactly one descriptor, got %d", fds)
		return nil, false, errors.Join(validationErr, closeActivationDescriptors(fds))
	}
	if !hasNames || namesValue != expectedName {
		validationErr := fmt.Errorf("systemd activation descriptor must be named %q, got %q", expectedName, namesValue)
		return nil, false, errors.Join(validationErr, closeActivationDescriptors(fds))
	}

	file := os.NewFile(systemdFirstFD, expectedName)
	if file == nil {
		return nil, false, errors.New("systemd activation descriptor 3 is unavailable")
	}
	listener, listenerErr := net.FileListener(file)
	closeErr := file.Close()
	if listenerErr != nil {
		return nil, false, fmt.Errorf("consume systemd activation descriptor 3: %w", errors.Join(listenerErr, closeErr))
	}
	if closeErr != nil {
		_ = listener.Close()
		return nil, false, fmt.Errorf("close systemd activation descriptor 3: %w", closeErr)
	}
	if _, ok := listener.(*net.UnixListener); !ok {
		_ = listener.Close()
		return nil, false, fmt.Errorf("systemd activation descriptor %q is not a Unix listener", expectedName)
	}
	return listener, true, nil
}

func closeActivationDescriptors(count int) error {
	var errs []error
	for fd := systemdFirstFD; fd < systemdFirstFD+count; fd++ {
		file := os.NewFile(uintptr(fd), fmt.Sprintf("systemd-activation-%d", fd))
		if file == nil {
			errs = append(errs, fmt.Errorf("consume systemd activation descriptor %d: unavailable", fd))
			continue
		}
		if err := file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("consume systemd activation descriptor %d: %w", fd, err))
		}
	}
	return errors.Join(errs...)
}

func unsetActivationEnvironment() error {
	var errs []error
	for _, name := range activationEnvironment {
		if err := os.Unsetenv(name); err != nil {
			errs = append(errs, fmt.Errorf("unset %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func configuredListener(path string) (net.Listener, error) {
	if strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("chat socket path must be absolute and clean: %q", path)
	}
	canonicalPath, err := preparePrivateSocketPath(path, os.Geteuid())
	if err != nil {
		return nil, fmt.Errorf("prepare chat socket parent: %w", err)
	}
	path = canonicalPath
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("chat socket path exists and is not a socket: %s", path)
		}
		if err := removeOwnedSocket(path, info); err != nil {
			return nil, fmt.Errorf("remove stale chat socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect chat socket path: %w", err)
	}

	unixListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on chat socket: %w", err)
	}
	unixListener.SetUnlinkOnClose(false)
	owner, err := os.Lstat(path)
	if err != nil {
		closeErr := unixListener.Close()
		return nil, errors.Join(fmt.Errorf("capture created chat socket ownership: %w", err), closeErr)
	}
	listener := &removingListener{Listener: unixListener, path: path, owner: owner}
	if err := configureSocketMode(path, owner, 0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("set chat socket mode: %w", err), listener.Close())
	}
	return listener, nil
}

var configureSocketMode = chmodOwnedSocket

func chmodOwnedSocket(path string, owner os.FileInfo, mode os.FileMode) error {
	if err := verifyOwnedSocket(path, owner); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	if err := verifyOwnedSocket(path, owner); err != nil {
		return err
	}
	return nil
}

func removeOwnedSocket(path string, owner os.FileInfo) error {
	if err := verifyOwnedSocket(path, owner); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func verifyOwnedSocket(path string, owner os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSocket == 0 || !os.SameFile(owner, current) {
		return errors.New("chat socket path changed ownership; refusing pathname mutation")
	}
	return nil
}

// preparePrivateSocketPath resolves only protected symlinks, validates both
// the requested and canonical ancestor chains, creates missing directories on
// the canonical path, and returns the only pathname callers may mutate.
func preparePrivateSocketPath(requestedPath string, effectiveUID int) (string, error) {
	requestedParent := filepath.Dir(requestedPath)
	relativeParent, err := filepath.Rel(string(filepath.Separator), requestedParent)
	if err != nil {
		return "", err
	}
	components := strings.Split(relativeParent, string(filepath.Separator))
	if relativeParent == "." {
		components = nil
	}

	canonicalParent := string(filepath.Separator)
	rootInfo, err := os.Lstat(canonicalParent)
	if err != nil {
		return "", err
	}
	if err := validateTrustedAncestor(canonicalParent, rootInfo, effectiveUID); err != nil {
		return "", err
	}

	for index, component := range components {
		candidate := filepath.Join(canonicalParent, component)
		info, statErr := os.Lstat(candidate)
		if errors.Is(statErr, os.ErrNotExist) {
			canonicalParent, err = createPrivateComponents(canonicalParent, components[index:], effectiveUID)
			if err != nil {
				return "", err
			}
			break
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if index == len(components)-1 {
				return "", fmt.Errorf("chat socket parent must not be a symlink: %s", requestedParent)
			}
			if err := validateTrustedSymlink(candidate, info, effectiveUID); err != nil {
				return "", err
			}
			resolved, resolveErr := filepath.EvalSymlinks(candidate)
			if resolveErr != nil {
				return "", fmt.Errorf("resolve trusted ancestor symlink %s: %w", candidate, resolveErr)
			}
			if err := validateCanonicalAncestorChain(resolved, effectiveUID); err != nil {
				return "", err
			}
			canonicalParent = resolved
			continue
		}
		if err := validateTrustedAncestor(candidate, info, effectiveUID); err != nil {
			return "", err
		}
		canonicalParent = candidate
	}

	if err := validatePrivateParent(canonicalParent, effectiveUID); err != nil {
		return "", err
	}
	canonicalInfo, err := os.Lstat(canonicalParent)
	if err != nil {
		return "", err
	}
	if err := verifyRequestedParent(requestedParent, canonicalParent, canonicalInfo); err != nil {
		return "", err
	}
	return filepath.Join(canonicalParent, filepath.Base(requestedPath)), nil
}

func createPrivateComponents(parent string, components []string, effectiveUID int) (string, error) {
	current := parent
	for _, component := range components {
		current = filepath.Join(current, component)
		if err := os.Mkdir(current, 0o700); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return "", err
			}
			if err := validatePrivateParent(current, effectiveUID); err != nil {
				return "", fmt.Errorf("refuse raced parent component: %w", err)
			}
			continue
		}
		if err := os.Chmod(current, 0o700); err != nil {
			return "", err
		}
		if err := validatePrivateParent(current, effectiveUID); err != nil {
			return "", err
		}
	}
	return current, nil
}

func validateCanonicalAncestorChain(path string, effectiveUID int) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("resolved chat socket ancestor is not absolute: %s", path)
	}
	relative, err := filepath.Rel(string(filepath.Separator), path)
	if err != nil {
		return err
	}
	current := string(filepath.Separator)
	components := strings.Split(relative, string(filepath.Separator))
	if relative == "." {
		components = nil
	}
	for index := -1; index < len(components); index++ {
		if index >= 0 {
			current = filepath.Join(current, components[index])
		}
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("canonical chat socket ancestor must not be a symlink: %s", current)
		}
		if err := validateTrustedAncestor(current, info, effectiveUID); err != nil {
			return err
		}
	}
	return nil
}

func validateTrustedAncestor(path string, info os.FileInfo, effectiveUID int) error {
	if !info.IsDir() {
		return fmt.Errorf("chat socket ancestor must be a directory: %s", path)
	}
	uid, err := fileUID(path, info)
	if err != nil {
		return err
	}
	if uid != 0 && uid != uint64(effectiveUID) {
		return fmt.Errorf("chat socket ancestor must be owned by root or effective uid %d, got %d: %s", effectiveUID, uid, path)
	}
	if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("group- or other-writable chat socket ancestor must have the sticky bit: %s", path)
	}
	return nil
}

func validateTrustedSymlink(path string, info os.FileInfo, effectiveUID int) error {
	uid, err := fileUID(path, info)
	if err != nil {
		return err
	}
	if uid != 0 && uid != uint64(effectiveUID) {
		return fmt.Errorf("chat socket ancestor symlink must be owned by root or effective uid %d, got %d: %s", effectiveUID, uid, path)
	}
	return nil
}

func fileUID(path string, info os.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("chat socket path ownership is unavailable: %s", path)
	}
	return uint64(stat.Uid), nil
}

func verifyRequestedParent(requestedParent, canonicalParent string, owner os.FileInfo) error {
	resolved, err := filepath.EvalSymlinks(requestedParent)
	if err != nil {
		return fmt.Errorf("re-resolve requested chat socket parent: %w", err)
	}
	requestedInfo, err := os.Stat(requestedParent)
	if err != nil {
		return fmt.Errorf("re-inspect requested chat socket parent: %w", err)
	}
	canonicalInfo, err := os.Lstat(canonicalParent)
	if err != nil {
		return fmt.Errorf("re-inspect canonical chat socket parent: %w", err)
	}
	if filepath.Clean(resolved) != filepath.Clean(canonicalParent) || !os.SameFile(owner, requestedInfo) || !os.SameFile(owner, canonicalInfo) {
		return errors.New("chat socket parent changed ownership; refusing pathname mutation")
	}
	return nil
}

// validatePrivateParent establishes the standalone authorization boundary.
// The final parent is private to the effective service UID, so only the same
// UID can replace stale socket names or race pathname-based ownership checks.
func validatePrivateParent(parent string, effectiveUID int) error {
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("chat socket parent must not be a symlink: %s", parent)
	}
	if !info.IsDir() {
		return fmt.Errorf("chat socket parent must be a directory: %s", parent)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		return fmt.Errorf("chat socket parent must have mode 0700, got %#o: %s", got, parent)
	}
	uid, err := fileUID(parent, info)
	if err != nil {
		return err
	}
	if uid != uint64(effectiveUID) {
		return fmt.Errorf("chat socket parent must be owned by effective uid %d, got %d: %s", effectiveUID, uid, parent)
	}
	return nil
}

type removingListener struct {
	net.Listener
	path  string
	owner os.FileInfo
	once  sync.Once
	err   error
}

func (l *removingListener) Close() error {
	l.once.Do(func() {
		closeErr := l.Listener.Close()
		if errors.Is(closeErr, net.ErrClosed) {
			closeErr = nil
		}
		removeErr := removeOwnedSocket(l.path, l.owner)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		l.err = errors.Join(closeErr, removeErr)
	})
	return l.err
}
