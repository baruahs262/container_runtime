package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/baruahs262/container-runtime/internal/cgroup"
)

// Runtime owns the on-disk container store.
type Runtime struct {
	Root         string // e.g. /var/lib/crt
	CgroupMount  string // e.g. /sys/fs/cgroup
	CgroupParent string // e.g. crt
}

// New returns a runtime rooted at root with default cgroup settings.
func New(root string) *Runtime {
	return &Runtime{Root: root, CgroupMount: cgroup.DefaultMount, CgroupParent: cgroup.DefaultParent}
}

// Dir is the container's private directory.
func (r *Runtime) Dir(id string) string { return filepath.Join(r.Root, "containers", id) }

// LogPath is where a detached container's output goes.
func (r *Runtime) LogPath(id string) string { return filepath.Join(r.Dir(id), "container.log") }

// Cgroup returns the manager for a container's cgroup.
func (r *Runtime) Cgroup(id string) (cgroup.Manager, error) {
	return cgroup.New(r.CgroupMount, r.CgroupParent, id)
}

// Prepare assigns an id and defaults and creates the container directory
// layout. It is idempotent.
func (r *Runtime) Prepare(s *Spec) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	if len(s.Args) == 0 {
		return errors.New("container: no command to run")
	}
	if s.Isolation == "" {
		s.Isolation = IsolationPivot
	}
	if s.Isolation != IsolationPivot && s.Isolation != IsolationChroot {
		return fmt.Errorf("container: unknown isolation %q", s.Isolation)
	}
	if s.Network == "" {
		s.Network = NetworkHost
	}
	if s.Network != NetworkHost && s.Network != NetworkNone {
		return fmt.Errorf("container: unknown network %q", s.Network)
	}
	if s.Hostname == "" {
		s.Hostname = s.ID
	}
	if s.WorkDir == "" {
		s.WorkDir = "/"
	}
	dir := r.Dir(s.ID)
	s.Upper = filepath.Join(dir, "upper")
	s.Work = filepath.Join(dir, "work")
	s.Rootfs = filepath.Join(dir, "rootfs")
	s.Empty = filepath.Join(dir, "empty")
	for _, d := range []string{s.Upper, s.Work, s.Rootfs, s.Empty} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) statePath(id string) string { return filepath.Join(r.Dir(id), "state.json") }

// Save writes state atomically.
func (r *Runtime) Save(st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	p := r.statePath(st.Spec.ID)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Load reads a container's state by exact id.
func (r *Runtime) Load(id string) (*State, error) {
	b, err := os.ReadFile(r.statePath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no such container: %s", id)
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("container %s: corrupt state: %w", id, err)
	}
	r.refresh(&st)
	return &st, nil
}

// refresh downgrades a stale "running" record whose processes are gone
// (for example the supervising crt process was SIGKILLed).
func (r *Runtime) refresh(st *State) {
	if st.Status != StatusRunning {
		return
	}
	if st.Pid > 0 && syscall.Kill(st.Pid, 0) == nil {
		return
	}
	if mgr, err := r.Cgroup(st.Spec.ID); err == nil {
		if pids, err := mgr.Pids(); err == nil && len(pids) > 0 {
			return
		}
	}
	st.Status = StatusDead
}

// Resolve finds a container by id or unique id prefix.
func (r *Runtime) Resolve(ref string) (*State, error) {
	if ref == "" {
		return nil, errors.New("empty container id")
	}
	if st, err := r.Load(ref); err == nil {
		return st, nil
	}
	ents, _ := os.ReadDir(filepath.Join(r.Root, "containers"))
	var match []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ref) {
			match = append(match, e.Name())
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("no such container: %s", ref)
	case 1:
		return r.Load(match[0])
	default:
		return nil, fmt.Errorf("ambiguous container id %q matches %d containers", ref, len(match))
	}
}

// List returns every container, newest first.
func (r *Runtime) List() ([]*State, error) {
	ents, err := os.ReadDir(filepath.Join(r.Root, "containers"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*State
	for _, e := range ents {
		if st, err := r.Load(e.Name()); err == nil {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Signal delivers sig to the container's init process.
func (r *Runtime) Signal(ref string, sig syscall.Signal) error {
	st, err := r.Resolve(ref)
	if err != nil {
		return err
	}
	if st.Status != StatusRunning {
		return fmt.Errorf("container %s is %s", st.Spec.ID, st.Status)
	}
	if sig == syscall.SIGKILL {
		mgr, err := r.Cgroup(st.Spec.ID)
		if err != nil {
			return err
		}
		return mgr.Kill()
	}
	return syscall.Kill(st.Pid, sig)
}

// Stop sends SIGTERM, waits up to timeout for the container to exit, then
// kills everything left in its cgroup.
func (r *Runtime) Stop(ref string, timeout time.Duration) error {
	st, err := r.Resolve(ref)
	if err != nil {
		return err
	}
	if st.Status != StatusRunning {
		return nil
	}
	id := st.Spec.ID
	mgr, err := r.Cgroup(id)
	if err != nil {
		return err
	}
	_ = syscall.Kill(st.Pid, syscall.SIGTERM)
	if r.waitExit(id, mgr, timeout) {
		return nil
	}
	if err := mgr.Kill(); err != nil {
		return err
	}
	r.waitExit(id, mgr, 5*time.Second)
	return nil
}

// waitExit polls until the cgroup is empty and the supervisor has
// recorded the exit, or timeout passes.
func (r *Runtime) waitExit(id string, mgr cgroup.Manager, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		pids, err := mgr.Pids()
		empty := err != nil || len(pids) == 0
		if empty {
			if st, err := r.Load(id); err != nil || st.Status != StatusRunning {
				return true
			}
		}
		if time.Now().After(deadline) {
			return empty
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Remove deletes a container's directory and cgroup. A running container
// is refused unless force is set, in which case it is killed first.
func (r *Runtime) Remove(ref string, force bool) error {
	st, err := r.Resolve(ref)
	if err != nil {
		return err
	}
	id := st.Spec.ID
	mgr, cgErr := r.Cgroup(id)
	if st.Status == StatusRunning {
		if !force {
			return fmt.Errorf("container %s is running; stop it or use --force", id)
		}
		if cgErr == nil {
			if err := mgr.Kill(); err != nil {
				return err
			}
			r.waitExit(id, mgr, 5*time.Second)
		}
	}
	if cgErr == nil {
		_ = mgr.Destroy()
	}
	return os.RemoveAll(r.Dir(id))
}

// Prune removes every container that is not running.
func (r *Runtime) Prune() ([]string, error) {
	all, err := r.List()
	if err != nil {
		return nil, err
	}
	var gone []string
	for _, st := range all {
		if st.Status == StatusRunning {
			continue
		}
		if err := r.Remove(st.Spec.ID, false); err != nil {
			return gone, err
		}
		gone = append(gone, st.Spec.ID)
	}
	return gone, nil
}
