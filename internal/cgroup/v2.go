package cgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// V2 manages a container in the unified cgroup v2 hierarchy.
type V2 struct {
	mount, parent, name string
	dir                 string
}

// NewV2 returns a manager for mount/parent/name.
func NewV2(mount, parent, name string) *V2 {
	return &V2{mount: mount, parent: parent, name: name,
		dir: filepath.Join(mount, parent, name)}
}

func (m *V2) Version() int { return 2 }
func (m *V2) Path() string { return m.dir }
func (m *V2) Dir() string  { return m.dir }

// enableControllers turns on cpu, memory and pids for children of dir.
// Controllers that the parent does not offer are skipped.
func enableControllers(dir string) error {
	avail, err := readString(dir, "cgroup.controllers")
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, c := range strings.Fields(avail) {
		have[c] = true
	}
	var want []string
	for _, c := range []string{"cpu", "memory", "pids"} {
		if have[c] {
			want = append(want, "+"+c)
		}
	}
	if len(want) == 0 {
		return nil
	}
	return writeFile(dir, "cgroup.subtree_control", strings.Join(want, " "))
}

func (m *V2) Create() error {
	parent := filepath.Join(m.mount, m.parent)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("cgroup: mkdir %s: %w", parent, err)
	}
	if err := enableControllers(m.mount); err != nil {
		return err
	}
	if err := enableControllers(parent); err != nil {
		return err
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return fmt.Errorf("cgroup: mkdir %s: %w", m.dir, err)
	}
	return nil
}

func (m *V2) Set(l Limits) error {
	if l.CPU > 0 {
		q := int64(l.CPU * CFSPeriod)
		if q < 1000 {
			q = 1000
		}
		if err := writeFile(m.dir, "cpu.max", fmt.Sprintf("%d %d", q, CFSPeriod)); err != nil {
			return err
		}
	}
	if l.Memory > 0 {
		if err := writeFile(m.dir, "memory.max", strconv.FormatInt(l.Memory, 10)); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(m.dir, "memory.swap.max")); err == nil {
			if err := writeFile(m.dir, "memory.swap.max", "0"); err != nil {
				return err
			}
		}
	}
	if l.Pids > 0 {
		if err := writeFile(m.dir, "pids.max", strconv.FormatInt(l.Pids, 10)); err != nil {
			return err
		}
	}
	return nil
}

func (m *V2) Apply(pid int) error {
	return writeFile(m.dir, "cgroup.procs", strconv.Itoa(pid))
}

func (m *V2) Stats() (Stats, error) {
	var s Stats
	kv, err := readKV(m.dir, "cpu.stat")
	if err != nil {
		return s, err
	}
	s.CPUUsageNs = kv["usage_usec"] * 1000
	s.NrPeriods = kv["nr_periods"]
	s.NrThrottled = kv["nr_throttled"]
	s.ThrottledNs = kv["throttled_usec"] * 1000

	s.MemUsage, _ = readUint(m.dir, "memory.current")
	s.MemMaxUsage, _ = readUint(m.dir, "memory.peak")
	s.MemLimit, _ = readUint(m.dir, "memory.max")
	if ev, err := readKV(m.dir, "memory.events"); err == nil {
		s.MemFailcnt = ev["max"]
		s.OOMKills = ev["oom_kill"]
	}
	s.Pids, _ = readUint(m.dir, "pids.current")
	s.PidsLimit, _ = readUint(m.dir, "pids.max")
	return s, nil
}

func (m *V2) Pids() ([]int, error) { return readPids(m.dir) }

func (m *V2) Kill() error {
	// cgroup.kill (Linux 5.14+) kills the whole subtree atomically.
	if _, err := os.Stat(filepath.Join(m.dir, "cgroup.kill")); err == nil {
		if err := writeFile(m.dir, "cgroup.kill", "1"); err != nil {
			return err
		}
	} else {
		_ = writeFile(m.dir, "cgroup.freeze", "1")
		defer writeFile(m.dir, "cgroup.freeze", "0")
	}
	return killAll(m.dir)
}

func (m *V2) Destroy() error { return rmdir(m.dir) }

// OOMEvents watches memory.events with inotify and forwards an event each
// time the oom_kill counter increases.
func (m *V2) OOMEvents(ctx context.Context) (<-chan struct{}, error) {
	f, err := inotifyWatch(filepath.Join(m.dir, "memory.events"))
	if err != nil {
		return nil, err
	}
	ch := make(chan struct{}, 8)
	var last uint64
	if ev, err := readKV(m.dir, "memory.events"); err == nil {
		last = ev["oom_kill"]
	}
	go func() {
		<-ctx.Done()
		f.Close()
	}()
	go func() {
		defer close(ch)
		buf := make([]byte, 4096)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
			ev, err := readKV(m.dir, "memory.events")
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return
				}
				continue
			}
			for n := ev["oom_kill"]; last < n; last++ {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}()
	return ch, nil
}
