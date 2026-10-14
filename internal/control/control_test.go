package control

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gitlab.com/integral-inference/voice-io/internal/paths"
)

// isolate gives the test its own home under a short path: Unix socket
// paths are limited to about 100 bytes.
func isolate(t *testing.T) {
	t.Helper()
	base := "/tmp"
	if runtime.GOOS == "windows" {
		base = os.TempDir()
	}
	home, err := os.MkdirTemp(base, "vio-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("LOCALAPPDATA", home)
}

func TestOneDaemonAnswersCommandsPrivately(t *testing.T) {
	isolate(t)
	if Running() {
		t.Fatal("a daemon is already answering")
	}
	if _, err := Send("push", ""); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("send without daemon: %v", err)
	}
	srv, err := Listen(func(r Request) (any, error) {
		switch r.Cmd {
		case "status":
			return map[string]string{"state": "IDLE", "arg": r.Arg}, nil
		case "explode":
			return nil, errors.New("unknown command: explode")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if _, err := Listen(func(Request) (any, error) { return nil, nil }); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second daemon: %v", err)
	}
	resp, err := Send("status", "x")
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]string
	if err := json.Unmarshal(resp.Data, &data); err != nil || data["state"] != "IDLE" || data["arg"] != "x" {
		t.Fatalf("status reply %s (%v)", resp.Data, err)
	}
	if _, err := Send("explode", ""); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("handler error: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(paths.SocketPath())
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("socket mode %v (%v)", info.Mode().Perm(), err)
		}
		if dir, _ := os.Stat(filepath.Dir(paths.SocketPath())); dir.Mode().Perm()&0o077 != 0 {
			t.Fatalf("socket dir mode %v", dir.Mode().Perm())
		}
	}

	c, err := net.Dial("unix", paths.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("not json\n"))
	buf := make([]byte, 256)
	n, _ := c.Read(buf)
	c.Close()
	if !strings.Contains(string(buf[:n]), "malformed request") {
		t.Fatalf("malformed reply %q", buf[:n])
	}
}

func TestStaleSocketIsReplacedAfterACrash(t *testing.T) {
	isolate(t)
	srv, err := Listen(func(Request) (any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	// A crash leaves the socket file behind but releases the lock.
	srv.ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = srv.ln.Close()
	_ = srv.lock.Close()
	if _, err := os.Stat(paths.SocketPath()); err != nil {
		t.Fatalf("stale socket missing: %v", err)
	}
	next, err := Listen(func(Request) (any, error) { return "pong", nil })
	if err != nil {
		t.Fatalf("restart after crash: %v", err)
	}
	defer next.Close()
	if !Running() {
		t.Fatal("restarted daemon not answering")
	}
}
