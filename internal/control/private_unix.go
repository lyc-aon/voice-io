//go:build !windows

package control

import (
	"net"
	"os"
	"syscall"
)

// listenPrivate creates the socket readable and writable by its owner only.
func listenPrivate(path string) (net.Listener, error) {
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return ln, nil
}

// lockFile takes an exclusive lock on f without waiting.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
