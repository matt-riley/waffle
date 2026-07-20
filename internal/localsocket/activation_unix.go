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
	if err := createPrivateParent(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("create chat socket parent: %w", err)
	}
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

func createPrivateParent(parent string) error {
	var missing []string
	for current := parent; ; current = filepath.Dir(current) {
		var (
			info os.FileInfo
			err  error
		)
		if current == parent {
			info, err = os.Lstat(current)
		} else {
			info, err = os.Stat(current)
		}
		if err == nil {
			if current == parent && info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("chat socket parent must not be a symlink: %s", parent)
			}
			if !info.IsDir() {
				return fmt.Errorf("parent component is not a directory: %s", current)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		next := filepath.Dir(current)
		if next == current {
			return fmt.Errorf("no existing parent for %s", parent)
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		path := missing[i]
		if err := os.Mkdir(path, 0o700); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
			info, statErr := os.Lstat(path)
			if statErr != nil {
				return statErr
			}
			if !info.IsDir() {
				return fmt.Errorf("raced parent component is not a directory: %s", path)
			}
			continue
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	return validatePrivateParent(parent, os.Geteuid())
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
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("chat socket parent ownership is unavailable: %s", parent)
	}
	if uint64(stat.Uid) != uint64(effectiveUID) {
		return fmt.Errorf("chat socket parent must be owned by effective uid %d, got %d: %s", effectiveUID, stat.Uid, parent)
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
