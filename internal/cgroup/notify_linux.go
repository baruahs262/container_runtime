package cgroup

import (
	"fmt"
	"os"
	"syscall"
)

const efdCloexec = syscall.O_CLOEXEC
const efdNonblock = syscall.O_NONBLOCK

// eventfd returns a non-blocking eventfd wrapped in an *os.File. Because
// the descriptor is non-blocking, os.File registers it with the runtime
// poller, so Close from another goroutine unblocks a pending Read.
func eventfd() (*os.File, error) {
	fd, _, errno := syscall.Syscall(syscall.SYS_EVENTFD2, 0, efdCloexec|efdNonblock, 0)
	if errno != 0 {
		return nil, fmt.Errorf("cgroup: eventfd2: %w", errno)
	}
	return os.NewFile(fd, "eventfd"), nil
}

// inotifyWatch returns a pollable inotify descriptor watching path for
// modification.
func inotifyWatch(path string) (*os.File, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("cgroup: inotify_init1: %w", err)
	}
	if _, err := syscall.InotifyAddWatch(fd, path, syscall.IN_MODIFY); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("cgroup: inotify_add_watch %s: %w", path, err)
	}
	return os.NewFile(uintptr(fd), "inotify"), nil
}
