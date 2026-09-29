package cgroup

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func writeFile(dir, name, val string) error {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(val), 0o644); err != nil {
		return fmt.Errorf("cgroup: write %s=%q: %w", p, val, err)
	}
	return nil
}

func readString(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// readUint parses a single-value file. "max" maps to 0 (unlimited).
func readUint(dir, name string) (uint64, error) {
	s, err := readString(dir, name)
	if err != nil {
		return 0, err
	}
	if s == "max" || s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

// readKV parses "key value" lines (cpu.stat, memory.events, memory.stat...).
func readKV(dir, name string) (map[string]uint64, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	return parseKV(b), nil
}

func parseKV(b []byte) map[string]uint64 {
	m := make(map[string]uint64)
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
			m[f[0]] = v
		}
	}
	return m
}

func readPids(dir string) ([]int, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		if p, err := strconv.Atoi(f); err == nil {
			pids = append(pids, p)
		}
	}
	return pids, nil
}

// killAll SIGKILLs every pid listed in dir/cgroup.procs until the list is
// empty or the deadline passes.
func killAll(dir string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		pids, err := readPids(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		for _, p := range pids {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cgroup: %d processes survive SIGKILL in %s", len(pids), dir)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// rmdir removes a cgroup directory, retrying while the kernel still
// considers it busy (tasks exiting asynchronously).
func rmdir(dir string) error {
	var err error
	for i := 0; i < 50; i++ {
		err = syscall.Rmdir(dir)
		if err == nil || errors.Is(err, syscall.ENOENT) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Plain directories (unit tests on tmpfs) are not auto-emptied.
	if errors.Is(err, syscall.ENOTEMPTY) {
		return os.RemoveAll(dir)
	}
	return fmt.Errorf("cgroup: rmdir %s: %w", dir, err)
}
