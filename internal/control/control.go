// Package control is the daemon's local control socket: newline-delimited
// JSON requests over a private Unix socket. Desktop shortcuts, the CLI and
// Settings use it to send push/toggle/cancel and to read status.
package control

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/paths"
)

// Request is one command.
type Request struct {
	Cmd string `json:"cmd"`
	Arg string `json:"arg,omitempty"`
}

// Response answers a Request.
type Response struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// ErrNotRunning means no daemon answered.
var ErrNotRunning = errors.New("voice-io is not running")

// ErrAlreadyRunning means another daemon owns the socket.
var ErrAlreadyRunning = errors.New("voice-io is already running")

// Handler executes one request and returns data to encode (may be nil).
type Handler func(Request) (any, error)

// Server accepts control connections.
type Server struct {
	ln   net.Listener
	lock *os.File
	path string
}

// Listen claims the single-instance lock and the socket.
func Listen(handler Handler) (*Server, error) {
	path := paths.SocketPath()
	if err := paths.PrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	lock, err := acquireLock(paths.LockPath())
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, 500*time.Millisecond); err == nil {
			c.Close()
			lock.Close()
			return nil, ErrAlreadyRunning
		}
		_ = os.Remove(path) // stale socket from a crashed daemon
	}
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		lock.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	s := &Server{ln: ln, lock: lock, path: path}
	go s.serve(handler)
	return s, nil
}

func acquireLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrAlreadyRunning
	}
	return f, nil
}

func (s *Server) serve(handler Handler) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			line, err := bufio.NewReader(conn).ReadBytes('\n')
			if err != nil && len(line) == 0 {
				return
			}
			var req Request
			resp := Response{OK: true}
			if err := json.Unmarshal(line, &req); err != nil || req.Cmd == "" {
				resp = Response{Error: "malformed request"}
			} else if data, err := handler(req); err != nil {
				resp = Response{Error: err.Error()}
			} else if data != nil {
				resp.Data, _ = json.Marshal(data)
			}
			out, _ := json.Marshal(resp)
			_, _ = conn.Write(append(out, '\n'))
		}()
	}
}

// Close stops accepting connections and releases the instance lock.
func (s *Server) Close() {
	_ = s.ln.Close()
	_ = os.Remove(s.path)
	_ = s.lock.Close()
}

// Send delivers one command to the running daemon.
func Send(cmd, arg string) (Response, error) {
	conn, err := net.DialTimeout("unix", paths.SocketPath(), time.Second)
	if err != nil {
		return Response{}, ErrNotRunning
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	req, _ := json.Marshal(Request{Cmd: cmd, Arg: arg})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return Response{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return Response{}, fmt.Errorf("no reply from voice-io: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, errors.New("malformed reply from voice-io")
	}
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// Running reports whether a daemon answers on the socket.
func Running() bool {
	_, err := Send("ping", "")
	return err == nil
}
