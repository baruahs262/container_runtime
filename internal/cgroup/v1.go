package cgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// v1Controllers are the hierarchies a container joins under cgroup v1.
// freezer is optional; the others are required when a limit uses them.
var v1Controllers = []string{"cpu", "cpuacct", "memory", "pids", "freezer"}

// V1 manages a container spread across per-controller v1 hierarchies.
type V1 struct {
	mount, parent, name string
	dirs                map[string]string // controller -> cgroup dir
}

// NewV1 returns a manager for mount/<controller>/parent/name.
func NewV1(mount, parent, name string) *V1 {
	m := &V1{mount: mount, parent: parent, name: name, dirs: map[string]string{}}
	for _, c := range v1Controllers {
		root := filepath.Join(mount, c)
		if _, err := os.Stat(root); err != nil {
			continue
		}
		// Hosts that co-mount cpu,cpuacct expose cpu and cpuacct as
		// symlinks to the same place; that is fine, dirs will collide and
		// we de-duplicate in uniqueDirs.
		m.dirs[c] = filepath.Join(root, parent, name)
	}
	return m
}

func (m *V1) Version() int { return 1 }

func (m *V1) Path() string {
	return filepath.Join(m.mount, "{"+joinKeys(m.dirs)+"}", m.parent, m.name)
}

func joinKeys(d map[string]string) string {
	s := ""
	for _, c := range v1Controllers {
		if _, ok := d[c]; ok {
			if s != "" {
				s += ","
			}
			s += c
		}
	}
	return s
}

// uniqueDirs returns each distinct cgroup directory once, resolving
// symlinked co-mounted hierarchies.
func (m *V1) uniqueDirs() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range v1Controllers {
		d, ok := m.dirs[c]
		if !ok {
			continue
		}
		key := d
		if r, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(d))); err == nil {
			key = filepath.Join(r, m.parent, m.name)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

func (m *V1) Create() error {
	if len(m.dirs) == 0 {
		return ErrNoCgroups
	}
	for _, d := range m.uniqueDirs() {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("cgroup: mkdir %s: %w", d, err)
		}
	}
	return nil
}

func (m *V1) dir(c string) (string, error) {
	d, ok := m.dirs[c]
	if !ok {
		return "", fmt.Errorf("cgroup: v1 controller %q not mounted", c)
	}
	return d, nil
}

// CFSPeriod is the scheduler period used for CPU quotas, in microseconds.
const CFSPeriod = 100000

func (m *V1) Set(l Limits) error {
	if l.CPU > 0 {
		d, err := m.dir("cpu")
		if err != nil {
			return err
		}
		if err := writeFile(d, "cpu.cfs_period_us", strconv.Itoa(CFSPeriod)); err != nil {
			return err
		}
		q := int64(l.CPU * CFSPeriod)
		if q < 1000 {
			q = 1000 // kernel minimum is 1ms
		}
		if err := writeFile(d, "cpu.cfs_quota_us", strconv.FormatInt(q, 10)); err != nil {
			return err
		}
	}
	if l.Memory > 0 {
		d, err := m.dir("memory")
		if err != nil {
			return err
		}
		v := strconv.FormatInt(l.Memory, 10)
		if err := writeFile(d, "memory.limit_in_bytes", v); err != nil {
			return err
		}
		// Forbid swap so the limit is a hard ceiling. memsw only exists
		// when swap accounting is enabled.
		if _, err := os.Stat(filepath.Join(d, "memory.memsw.limit_in_bytes")); err == nil {
			if err := writeFile(d, "memory.memsw.limit_in_bytes", v); err != nil {
				return err
			}
		}
		if _, err := os.Stat(filepath.Join(d, "memory.swappiness")); err == nil {
			_ = writeFile(d, "memory.swappiness", "0")
		}
	}
	if l.Pids > 0 {
		d, err := m.dir("pids")
		if err != nil {
			return err
		}
		if err := writeFile(d, "pids.max", strconv.FormatInt(l.Pids, 10)); err != nil {
			return err
		}
	}
	return nil
}

func (m *V1) Apply(pid int) error {
	for _, d := range m.uniqueDirs() {
		if err := writeFile(d, "cgroup.procs", strconv.Itoa(pid)); err != nil {
			return err
		}
	}
	return nil
}

func (m *V1) Stats() (Stats, error) {
	var s Stats
	if d, ok := m.dirs["cpuacct"]; ok {
		s.CPUUsageNs, _ = readUint(d, "cpuacct.usage")
	}
	if d, ok := m.dirs["cpu"]; ok {
		if kv, err := readKV(d, "cpu.stat"); err == nil {
			s.NrPeriods = kv["nr_periods"]
			s.NrThrottled = kv["nr_throttled"]
			s.ThrottledNs = kv["throttled_time"]
		}
	}
	if d, ok := m.dirs["memory"]; ok {
		var err error
		if s.MemUsage, err = readUint(d, "memory.usage_in_bytes"); err != nil {
			return s, err
		}
		s.MemMaxUsage, _ = readUint(d, "memory.max_usage_in_bytes")
		s.MemLimit, _ = readUint(d, "memory.limit_in_bytes")
		s.MemFailcnt, _ = readUint(d, "memory.failcnt")
		if kv, err := readKV(d, "memory.oom_control"); err == nil {
			s.OOMKills = kv["oom_kill"]
		}
		// The kernel reports "no limit" as a huge page-aligned number.
		if s.MemLimit >= 1<<62 {
			s.MemLimit = 0
		}
	}
	if d, ok := m.dirs["pids"]; ok {
		s.Pids, _ = readUint(d, "pids.current")
		s.PidsLimit, _ = readUint(d, "pids.max")
	}
	return s, nil
}

func (m *V1) Pids() ([]int, error) {
	for _, d := range m.uniqueDirs() {
		return readPids(d)
	}
	return nil, ErrNoCgroups
}

func (m *V1) Kill() error {
	fz, hasFreezer := m.dirs["freezer"]
	if hasFreezer {
		// Freeze first so nothing can fork while we kill.
		if err := writeFile(fz, "freezer.state", "FROZEN"); err != nil {
			hasFreezer = false
		}
	}
	var errs []error
	for _, d := range m.uniqueDirs() {
		pids, _ := readPids(d)
		for _, p := range pids {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
	}
	if hasFreezer {
		_ = writeFile(fz, "freezer.state", "THAWED")
	}
	for _, d := range m.uniqueDirs() {
		if err := killAll(d); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *V1) Destroy() error {
	var errs []error
	for _, d := range m.uniqueDirs() {
		if err := rmdir(d); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// OOMEvents registers an eventfd with memory.oom_control via
// cgroup.event_control. The kernel signals the eventfd on every OOM and
// also when the cgroup is removed; only the former is forwarded, detected
// by the oom_kill counter moving (or under_oom being set on old kernels).
func (m *V1) OOMEvents(ctx context.Context) (<-chan struct{}, error) {
	d, err := m.dir("memory")
	if err != nil {
		return nil, err
	}
	ctl, err := os.Open(filepath.Join(d, "memory.oom_control"))
	if err != nil {
		return nil, err
	}
	efd, err := eventfd()
	if err != nil {
		ctl.Close()
		return nil, err
	}
	reg := fmt.Sprintf("%d %d", efd.Fd(), ctl.Fd())
	if err := writeFile(d, "cgroup.event_control", reg); err != nil {
		ctl.Close()
		efd.Close()
		return nil, err
	}
	ch := make(chan struct{}, 8)
	var last uint64
	if kv, err := readKV(d, "memory.oom_control"); err == nil {
		last = kv["oom_kill"]
	}
	go func() {
		<-ctx.Done()
		efd.Close() // unblocks the reader
	}()
	go func() {
		defer close(ch)
		defer ctl.Close()
		buf := make([]byte, 8)
		for {
			if _, err := efd.Read(buf); err != nil {
				return
			}
			kv, err := readKV(d, "memory.oom_control")
			if err != nil {
				return // cgroup removed
			}
			fire := 0
			if n := kv["oom_kill"]; n > last {
				fire = int(n - last)
				last = n
			} else if n == 0 && kv["under_oom"] == 1 {
				fire = 1 // kernel predates the oom_kill counter
			}
			for ; fire > 0; fire-- {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}()
	return ch, nil
}
