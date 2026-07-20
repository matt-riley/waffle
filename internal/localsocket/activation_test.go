//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package localsocket

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestInheritedListenerConsumesFD3AndUnsetsActivation(t *testing.T) {
	dir := unixTempDir(t)
	path := filepath.Join(dir, "inherited.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(path)
	})
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	file, err := listener.File()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `LISTEN_PID=$$ LISTEN_FDS=1 LISTEN_FDNAMES=waffle-chat WAFFLE_TEST_INHERITED_HELPER=1 WAFFLE_TEST_INHERITED_PATH="$2" exec "$1" -test.run '^TestInheritedListenerHelperProcess$'`, "sh", os.Args[0], path)
	cmd.ExtraFiles = []*os.File{file}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "READY" {
		_ = cmd.Process.Kill()
		t.Fatalf("helper readiness = %q, scan err=%v, stderr=%s", scanner.Text(), scanner.Err(), stderr.String())
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("dial inherited listener: %v", err)
	}
	_ = conn.Close()
	if !scanner.Scan() || scanner.Text() != "ACCEPTED" {
		_ = cmd.Process.Kill()
		t.Fatalf("helper accept = %q, scan err=%v, stderr=%s", scanner.Text(), scanner.Err(), stderr.String())
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper failed: %v: %s", err, stderr.String())
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o660 {
		t.Fatalf("inherited socket changed after Close: %v, %v", info, err)
	}
}

func TestInheritedListenerHelperProcess(t *testing.T) {
	if os.Getenv("WAFFLE_TEST_INHERITED_HELPER") != "1" {
		t.Skip("helper process")
	}
	listener, inherited, err := Listener("relative-must-not-be-used.sock")
	if err != nil {
		t.Fatal(err)
	}
	if listener == nil || !inherited {
		t.Fatalf("Listener = %v, %v; want inherited listener", listener, inherited)
	}
	defer func() { _ = listener.Close() }()
	for _, name := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		if value, ok := os.LookupEnv(name); ok {
			t.Fatalf("%s remained set to %q", name, value)
		}
	}
	info, err := os.Stat(os.Getenv("WAFFLE_TEST_INHERITED_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o660 {
		t.Fatalf("inherited socket mode = %#o, want 0660", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	if unixListener, ok := listener.(*net.UnixListener); ok {
		if err := unixListener.SetDeadline(deadline); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("READY")
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	fmt.Println("ACCEPTED")
}

func TestInheritedListenerMismatchedPIDIsIgnoredAndEnvironmentUnset(t *testing.T) {
	runActivationFDHelper(t, 1, "waffle-chat", true)
}

func TestInheritedListenerRejectsMultipleDescriptors(t *testing.T) {
	runActivationFDHelper(t, 2, "waffle-chat:other", false)
}

func TestInheritedListenerRejectsWrongDescriptorName(t *testing.T) {
	runActivationFDHelper(t, 1, "wrong-name", false)
}

func TestInheritedListenerRejectsDefensivelyUnboundedDescriptorCount(t *testing.T) {
	setActivationEnvironment(t, "65", "waffle-chat")
	listener, inherited, err := Listener("")
	if err == nil || !strings.Contains(err.Error(), "limit") || listener != nil || inherited {
		t.Fatalf("Listener = %v, %v, %v; want descriptor limit error", listener, inherited, err)
	}
	assertActivationEnvironmentUnset(t)
}

func TestInheritedListenerFDValidationHelperProcess(t *testing.T) {
	mode := os.Getenv("WAFFLE_TEST_ACTIVATION_FD_HELPER")
	if mode == "" {
		t.Skip("helper process")
	}
	count, err := strconv.Atoi(os.Getenv("WAFFLE_TEST_EXPECT_FDS"))
	if err != nil {
		t.Fatal(err)
	}
	listener, inherited, listenerErr := Listener("")
	if listener != nil || inherited {
		t.Fatalf("Listener = %v, %v; want no listener", listener, inherited)
	}
	if mode == "mismatch" {
		if listenerErr != nil {
			t.Fatalf("PID mismatch = %v, want ignored", listenerErr)
		}
	} else if listenerErr == nil {
		t.Fatal("descriptor validation unexpectedly succeeded")
	}
	for fd := systemdFirstFD; fd < systemdFirstFD+count; fd++ {
		_, fdErr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if mode == "mismatch" {
			if fdErr != nil {
				t.Fatalf("PID mismatch consumed fd %d: %v", fd, fdErr)
			}
		} else if !errors.Is(fdErr, unix.EBADF) {
			t.Fatalf("validation left fd %d open: %v", fd, fdErr)
		}
	}
	assertActivationEnvironmentUnset(t)
}

func TestInheritedListenerRejectsTCPDescriptor(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	file, err := listener.File()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `LISTEN_PID=$$ LISTEN_FDS=1 LISTEN_FDNAMES=waffle-chat WAFFLE_TEST_TCP_HELPER=1 exec "$1" -test.run '^TestInheritedListenerTCPHelperProcess$'`, "sh", os.Args[0])
	cmd.ExtraFiles = []*os.File{file}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v: %s", err, output)
	}
}

func TestInheritedListenerTCPHelperProcess(t *testing.T) {
	if os.Getenv("WAFFLE_TEST_TCP_HELPER") != "1" {
		t.Skip("helper process")
	}
	listener, inherited, err := Listener("")
	if err == nil || !strings.Contains(err.Error(), "Unix") || listener != nil || inherited {
		t.Fatalf("Listener = %v, %v, %v; want non-Unix descriptor refusal", listener, inherited, err)
	}
	assertActivationEnvironmentUnset(t)
}

func TestConfiguredUnixRejectsRelativeAndNonCleanPaths(t *testing.T) {
	for _, path := range []string{"chat.sock", filepath.Join(t.TempDir(), "nested", "..", "chat.sock")} {
		listener, inherited, err := Listener(path)
		if err == nil || listener != nil || inherited {
			t.Errorf("Listener(%q) = %v, %v, %v; want path validation error", path, listener, inherited, err)
		}
	}
}

func TestConfiguredUnixRejectsExistingNonSocketPaths(t *testing.T) {
	tests := []struct {
		name  string
		setup func(string) error
	}{
		{name: "regular", setup: func(path string) error { return os.WriteFile(path, []byte("do not remove"), 0o600) }},
		{name: "directory", setup: func(path string) error { return os.Mkdir(path, 0o700) }},
		{name: "symlink", setup: func(path string) error { return os.Symlink(filepath.Join(filepath.Dir(path), "missing"), path) }},
		{name: "fifo", setup: func(path string) error { return unix.Mkfifo(path, 0o600) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(unixTempDir(t), "chat.sock")
			if err := tt.setup(path); err != nil {
				t.Fatal(err)
			}
			listener, inherited, err := Listener(path)
			if err == nil || listener != nil || inherited {
				t.Fatalf("Listener = %v, %v, %v; want existing-path refusal", listener, inherited, err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("refused path was removed: %v", err)
			}
		})
	}
}

func TestConfiguredUnixReplacesStaleSocketAndOwnsModesAndRemoval(t *testing.T) {
	root := unixTempDir(t)
	parent := filepath.Join(root, "private")
	path := filepath.Join(parent, "chat.sock")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	listener, inherited, err := Listener(path)
	if err != nil {
		t.Fatal(err)
	}
	if listener == nil || inherited {
		t.Fatalf("Listener = %v, %v; want configured listener", listener, inherited)
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if got := parentInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("parent mode = %#o, want 0700", got)
	}
	socketInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want socket 0600", socketInfo.Mode())
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("dial configured listener: %v", err)
	}
	_ = conn.Close()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket path remained after close: %v", err)
	}
}

func TestConfiguredUnixCreatesPrivateParent(t *testing.T) {
	path := filepath.Join(unixTempDir(t), "private", "chat.sock")
	listener, _, err := Listener(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("created parent mode = %#o, want 0700", got)
	}
}

func TestConfiguredUnixCreatesParentAt0700DespiteUmask(t *testing.T) {
	path := filepath.Join(unixTempDir(t), "private", "chat.sock")
	oldUmask := unix.Umask(0o777)
	defer unix.Umask(oldUmask)
	listener, _, err := Listener(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("created parent mode under restrictive umask = %#o, want 0700", got)
	}
}

func TestConfiguredUnixRefusesPermissiveExistingParentWithoutChangingIt(t *testing.T) {
	root := unixTempDir(t)
	parent := filepath.Join(root, "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "chat.sock")
	listener, inherited, err := Listener(path)
	if err == nil || !strings.Contains(err.Error(), "0700") || listener != nil || inherited {
		t.Fatalf("Listener = %v, %v, %v; want private-parent refusal", listener, inherited, err)
	}
	info, statErr := os.Lstat(parent)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("refused parent mode changed to %#o", got)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused parent allowed socket path mutation: %v", statErr)
	}
}

func TestConfiguredUnixRefusesSymlinkedFinalParent(t *testing.T) {
	root := unixTempDir(t)
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "linked")
	if err := os.Symlink(target, parent); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "chat.sock")
	listener, inherited, err := Listener(path)
	if err == nil || !strings.Contains(err.Error(), "symlink") || listener != nil || inherited {
		t.Fatalf("Listener = %v, %v, %v; want symlink-parent refusal", listener, inherited, err)
	}
	info, statErr := os.Lstat(parent)
	if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("refused parent symlink changed: %v, %v", info, statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(target, "chat.sock")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("symlink parent allowed target mutation: %v", statErr)
	}
}

func TestConfiguredUnixLeavesPrivateExistingParentUnchanged(t *testing.T) {
	root := unixTempDir(t)
	parent := filepath.Join(root, "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(parent)
	if err != nil {
		t.Fatal(err)
	}
	listener, _, err := Listener(filepath.Join(parent, "chat.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || after.Mode().Perm() != 0o700 {
		t.Fatalf("private existing parent changed: before=%v after=%v", before.Mode(), after.Mode())
	}
}

func TestConfiguredUnixRejectsFinalParentOwnedByAnotherUID(t *testing.T) {
	parent := unixTempDir(t)
	err := validatePrivateParent(parent, os.Geteuid()+1)
	if err == nil || !strings.Contains(err.Error(), "owned") {
		t.Fatalf("validatePrivateParent = %v, want ownership refusal", err)
	}
}

func TestConfiguredUnixStaleRemovalRefusesReplacement(t *testing.T) {
	parent := filepath.Join(unixTempDir(t), "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "chat.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	owner, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	moved := path + ".stale"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = removeOwnedSocket(path, owner)
	if err == nil || !strings.Contains(err.Error(), "changed ownership") {
		t.Fatalf("removeOwnedSocket = %v, want ownership refusal", err)
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil || string(contents) != "replacement" {
		t.Fatalf("stale removal changed replacement: %q, %v", contents, readErr)
	}
}

func TestConfiguredUnixSetupCleanupRefusesReplacement(t *testing.T) {
	parent := filepath.Join(unixTempDir(t), "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "chat.sock")
	want := errors.New("forced socket mode failure")
	original := configureSocketMode
	configureSocketMode = func(path string, _ os.FileInfo, _ os.FileMode) error {
		if err := os.Rename(path, path+".created"); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte("replacement"), 0o644); err != nil {
			return err
		}
		return want
	}
	defer func() { configureSocketMode = original }()

	listener, inherited, err := Listener(path)
	if listener != nil || inherited || !errors.Is(err, want) || !strings.Contains(err.Error(), "changed ownership") {
		t.Fatalf("Listener = %v, %v, %v; want setup and ownership errors", listener, inherited, err)
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil || string(contents) != "replacement" {
		t.Fatalf("setup cleanup changed replacement: %q, %v", contents, readErr)
	}
	info, statErr := os.Lstat(path)
	if statErr != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("setup cleanup chmodded replacement: %v, %v", info, statErr)
	}
}

func TestConfiguredUnixCloseRefusesReplacementAndCachesFirstError(t *testing.T) {
	parent := filepath.Join(unixTempDir(t), "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "chat.sock")
	listener, _, err := Listener(path)
	if err != nil {
		t.Fatal(err)
	}
	moved := path + ".created"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	firstErr := listener.Close()
	if firstErr == nil || !strings.Contains(firstErr.Error(), "changed ownership") {
		t.Fatalf("first Close = %v, want ownership refusal", firstErr)
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil || string(contents) != "replacement" {
		t.Fatalf("Close changed replacement: %q, %v", contents, readErr)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, path); err != nil {
		t.Fatal(err)
	}
	secondErr := listener.Close()
	if secondErr == nil || secondErr.Error() != firstErr.Error() {
		t.Fatalf("second Close = %v, want cached %v", secondErr, firstErr)
	}
	info, statErr := os.Lstat(path)
	if statErr != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("second Close resumed removal: %v, %v", info, statErr)
	}
}

func setActivationEnvironment(t *testing.T, fds, names string) {
	t.Helper()
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LISTEN_FDS", fds)
	t.Setenv("LISTEN_FDNAMES", names)
}

func runActivationFDHelper(t *testing.T, count int, names string, pidMismatch bool) {
	t.Helper()
	extraFiles := make([]*os.File, 0, count)
	for range count {
		readFile, writeFile, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		extraFiles = append(extraFiles, readFile)
		t.Cleanup(func() {
			_ = readFile.Close()
			_ = writeFile.Close()
		})
	}
	mode := "reject"
	pidExpression := "$$"
	if pidMismatch {
		mode = "mismatch"
		pidExpression = "1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	script := `LISTEN_PID=` + pidExpression + ` LISTEN_FDS="$2" LISTEN_FDNAMES="$3" WAFFLE_TEST_ACTIVATION_FD_HELPER="$4" WAFFLE_TEST_EXPECT_FDS="$2" exec "$1" -test.run '^TestInheritedListenerFDValidationHelperProcess$'`
	cmd := exec.CommandContext(ctx, "sh", "-c", script, "sh", os.Args[0], strconv.Itoa(count), names, mode)
	cmd.ExtraFiles = extraFiles
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("activation fd helper failed: %v: %s", err, output)
	}
}

func assertActivationEnvironmentUnset(t *testing.T) {
	t.Helper()
	for _, name := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		if value, ok := os.LookupEnv(name); ok {
			t.Errorf("%s remained set to %q", name, value)
		}
	}
}

func unixTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "waffle-ls-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
