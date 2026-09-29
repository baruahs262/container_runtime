// Package cgroup creates, configures, inspects and destroys the control
// groups that bound a container's CPU, memory and process count.
//
// Both cgroup v1 (one hierarchy per controller) and the unified v2 hierarchy
// are supported behind the Manager interface. Every implementation is rooted
// at a configurable mount point so the file-writing logic can be unit-tested
// against a temporary directory.
package cgroup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// DefaultMount is where the kernel's cgroup filesystems usually live.
const DefaultMount = "/sys/fs/cgroup"

// DefaultParent is the directory under each hierarchy that holds all
// containers managed by this runtime.
const DefaultParent = "crt"

// Limits describes the resources a container may use. Zero means unlimited.
type Limits struct {
	CPU    float64 `json:"cpu,omitempty"`    // CPU cores, e.g. 0.5
	Memory int64   `json:"memory,omitempty"` // bytes
	Pids   int64   `json:"pids,omitempty"`   // max tasks
}

// Stats is a point-in-time snapshot of a cgroup's resource accounting.
type Stats struct {
	CPUUsageNs  uint64 `json:"cpu_usage_ns"`
	NrPeriods   uint64 `json:"nr_periods"`
	NrThrottled uint64 `json:"nr_throttled"`
	ThrottledNs uint64 `json:"throttled_ns"`

	MemUsage    uint64 `json:"mem_usage"`
	MemMaxUsage uint64 `json:"mem_max_usage"`
	MemLimit    uint64 `json:"mem_limit"`
	MemFailcnt  uint64 `json:"mem_failcnt"`
	OOMKills    uint64 `json:"oom_kills"`

	Pids      uint64 `json:"pids"`
	PidsLimit uint64 `json:"pids_limit"`
}

// Manager controls one container's cgroup.
type Manager interface {
	// Create makes the cgroup directories.
	Create() error
	// Set applies resource limits.
	Set(Limits) error
	// Apply moves a process into the cgroup.
	Apply(pid int) error
	// Stats reads current accounting.
	Stats() (Stats, error)
	// OOMEvents delivers one value per OOM kill until ctx is done.
	OOMEvents(ctx context.Context) (<-chan struct{}, error)
	// Pids lists every process in the cgroup.
	Pids() ([]int, error)
	// Kill sends SIGKILL to every process in the cgroup and waits until
	// the cgroup is empty.
	Kill() error
	// Destroy removes the cgroup directories.
	Destroy() error
	// Path is a human-readable location, used in logs and state files.
	Path() string
	// Version is 1 or 2.
	Version() int
}

// ErrNoCgroups is returned when no usable cgroup filesystem was found.
var ErrNoCgroups = errors.New("cgroup: no cgroup filesystem mounted")

// IsV2 reports whether mount holds a unified (v2) hierarchy.
func IsV2(mount string) bool {
	_, err := os.Stat(filepath.Join(mount, "cgroup.controllers"))
	return err == nil
}

// New returns a Manager for the cgroup named name (normally a container id)
// under parent, choosing the implementation the host provides.
func New(mount, parent, name string) (Manager, error) {
	if mount == "" {
		mount = DefaultMount
	}
	if parent == "" {
		parent = DefaultParent
	}
	if IsV2(mount) {
		return NewV2(mount, parent, name), nil
	}
	if _, err := os.Stat(filepath.Join(mount, "memory")); err == nil {
		return NewV1(mount, parent, name), nil
	}
	if _, err := os.Stat(filepath.Join(mount, "cpu")); err == nil {
		return NewV1(mount, parent, name), nil
	}
	return nil, ErrNoCgroups
}
