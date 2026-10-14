package control

import (
	"net"
	"os"

	"golang.org/x/sys/windows"
)

// listenPrivate creates the socket. It lives in the user's %LOCALAPPDATA%,
// whose inherited ACL already admits only that user and the system.
func listenPrivate(path string) (net.Listener, error) { return net.Listen("unix", path) }

// lockFile takes an exclusive lock on f without waiting.
func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
}
