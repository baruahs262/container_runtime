// Package container runs processes in isolated Linux namespaces on top of
// an overlay root filesystem, confined by cgroups.
//
// The runtime re-executes its own binary as the container's init process
// ("crt-init"): the parent clones it into new mount, UTS, IPC and PID (and
// optionally network) namespaces, places it into the container's cgroup,
// then releases it to build the root filesystem and exec the workload.
package container

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/baruahs262/container-runtime/internal/cgroup"
	"github.com/baruahs262/container-runtime/internal/rootfs"
)

// Isolation modes for switching into the container root.
const (
	IsolationPivot  = "pivot"  // pivot_root + detach old root (default)
	IsolationChroot = "chroot" // plain chroot, host root stays mounted
)

// Network modes.
const (
	NetworkHost = "host" // share the host network namespace
	NetworkNone = "none" // private namespace with only loopback
)

// Container states.
const (
	StatusCreated = "created"
	StatusRunning = "running"
	StatusExited  = "exited"
	StatusDead    = "dead" // recorded as running but nothing is alive
)

// Spec fully describes a container.
type Spec struct {
	ID         string        `json:"id"`
	Image      string        `json:"image,omitempty"`
	Layers     []string      `json:"layers"` // lower dirs, bottom-most first
	LayerIDs   []string      `json:"layer_ids,omitempty"`
	Args       []string      `json:"args"`
	Env        []string      `json:"env,omitempty"`
	WorkDir    string        `json:"workdir,omitempty"`
	Hostname   string        `json:"hostname,omitempty"`
	Limits     cgroup.Limits `json:"limits"`
	Isolation  string        `json:"isolation,omitempty"`
	Network    string        `json:"network,omitempty"`
	Binds      []rootfs.Bind `json:"binds,omitempty"`
	Init       bool          `json:"init,omitempty"`       // run a reaping, signal-forwarding PID 1
	Privileged bool          `json:"privileged,omitempty"` // keep the full capability bounding set
	ReadOnly   bool          `json:"readonly,omitempty"`   // read-only root filesystem

	// Filled in by Runtime.Prepare.
	Upper  string `json:"upper,omitempty"`
	Work   string `json:"work,omitempty"`
	Rootfs string `json:"rootfs,omitempty"`
	Empty  string `json:"empty,omitempty"`
}

// State is persisted to <root>/containers/<id>/state.json.
type State struct {
	Spec      *Spec     `json:"spec"`
	Status    string    `json:"status"`
	Pid       int       `json:"pid,omitempty"`
	ShimPid   int       `json:"shim_pid,omitempty"`
	ExitCode  int       `json:"exit_code"`
	OOMKilled bool      `json:"oom_killed,omitempty"`
	Error     string    `json:"error,omitempty"`
	Cgroup    string    `json:"cgroup,omitempty"`
	Detached  bool      `json:"detached,omitempty"`
	Created   time.Time `json:"created"`
	Started   time.Time `json:"started,omitempty"`
	Finished  time.Time `json:"finished,omitempty"`
}

// NewID returns a random 12-hex-digit container id.
func NewID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
