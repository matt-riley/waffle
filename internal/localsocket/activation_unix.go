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
)

const systemdFirstFD = 3

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
	if err != nil || fds != 1 {
		return nil, false, fmt.Errorf("systemd activation requires exactly one descriptor, got %q", fdsValue)
	}
	if !hasNames || namesValue != expectedName {
		return nil, false, fmt.Errorf("systemd activation descriptor must be named %q, got %q", expectedName, namesValue)
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
		if err := os.Remove(path); err != nil {
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
	cleanup := func(cause error) (net.Listener, error) {
		closeErr := unixListener.Close()
		removeErr := os.Remove(path)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		return nil, errors.Join(cause, closeErr, removeErr)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return cleanup(fmt.Errorf("set chat socket mode: %w", err))
	}
	owner, err := os.Lstat(path)
	if err != nil {
		return cleanup(fmt.Errorf("inspect created chat socket: %w", err))
	}
	return &removingListener{Listener: unixListener, path: path, owner: owner}, nil
}

func createPrivateParent(parent string) error {
	var missing []string
	for current := parent; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
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
		current, statErr := os.Lstat(l.path)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			statErr = nil
		case statErr != nil:
			statErr = fmt.Errorf("inspect owned chat socket during close: %w", statErr)
		case current.Mode()&os.ModeSocket == 0 || !os.SameFile(l.owner, current):
			statErr = errors.New("chat socket path changed ownership during close; refusing removal")
		default:
			statErr = os.Remove(l.path)
			if errors.Is(statErr, os.ErrNotExist) {
				statErr = nil
			}
		}
		l.err = errors.Join(closeErr, statErr)
	})
	return l.err
}
