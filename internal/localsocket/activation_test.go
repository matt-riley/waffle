//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package localsocket

import (
	"bufio"
	"bytes"
	"context"
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
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()+10000))
	t.Setenv("LISTEN_FDS", "1")
	t.Setenv("LISTEN_FDNAMES", "waffle-chat")

	listener, inherited, err := Listener("")
	if err != nil || listener != nil || inherited {
		t.Fatalf("Listener = %v, %v, %v; want ignored activation", listener, inherited, err)
	}
	assertActivationEnvironmentUnset(t)
}

func TestInheritedListenerRejectsMultipleDescriptors(t *testing.T) {
	setActivationEnvironment(t, "2", "waffle-chat:other")
	listener, inherited, err := Listener("")
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("Listener = %v, %v, %v; want descriptor-count error", listener, inherited, err)
	}
	assertActivationEnvironmentUnset(t)
}

func TestInheritedListenerRejectsWrongDescriptorName(t *testing.T) {
	setActivationEnvironment(t, "1", "wrong-name")
	listener, inherited, err := Listener("")
	if err == nil || !strings.Contains(err.Error(), "waffle-chat") {
		t.Fatalf("Listener = %v, %v, %v; want descriptor-name error", listener, inherited, err)
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

func setActivationEnvironment(t *testing.T, fds, names string) {
	t.Helper()
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LISTEN_FDS", fds)
	t.Setenv("LISTEN_FDNAMES", names)
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
