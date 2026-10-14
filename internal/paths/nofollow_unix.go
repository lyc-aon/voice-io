//go:build !windows

package paths

import "syscall"

// NoFollow makes os.OpenFile refuse a symlinked leaf.
const NoFollow = syscall.O_NOFOLLOW
