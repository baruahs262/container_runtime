package cgroup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func fakeV1(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, c := range []string{"cpu", "cpuacct", "memory", "pids"} {
		if err := os.MkdirAll(filepath.Join(root, c), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDetect(t *testing.T) {
	v1 := fakeV1(t)
	m, err := New(v1, "crt", "abc")
	if err != nil || m.Version() != 1 {
		t.Fatalf("v1 detect: %v %v", m, err)
	}
	v2 := t.TempDir()
	os.WriteFile(filepath.Join(v2, "cgroup.controllers"), []byte("cpu memory pids"), 0o644)
	m, err = New(v2, "crt", "abc")
	if err != nil || m.Version() != 2 {
		t.Fatalf("v2 detect: %v %v", m, err)
	}
	if _, err := New(t.TempDir(), "", "x"); err != ErrNoCgroups {
		t.Fatalf("empty mount: got %v", err)
	}
}

func TestV1SetWritesFiles(t *testing.T) {
	root := fakeV1(t)
	m := NewV1(root, "crt", "c1")
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}
	memDir := filepath.Join(root, "memory", "crt", "c1")
	// Pretend swap accounting exists so memsw is written too.
	os.WriteFile(filepath.Join(memDir, "memory.memsw.limit_in_bytes"), nil, 0o644)

	if err := m.Set(Limits{CPU: 0.5, Memory: 64 << 20, Pids: 32}); err != nil {
		t.Fatal(err)
	}
	cpu := filepath.Join(root, "cpu", "crt", "c1")
	if got := read(t, filepath.Join(cpu, "cpu.cfs_quota_us")); got != "50000" {
		t.Errorf("quota = %s", got)
	}
	if got := read(t, filepath.Join(cpu, "cpu.cfs_period_us")); got != "100000" {
		t.Errorf("period = %s", got)
	}
	if got := read(t, filepath.Join(memDir, "memory.limit_in_bytes")); got != "67108864" {
		t.Errorf("mem = %s", got)
	}
	if got := read(t, filepath.Join(memDir, "memory.memsw.limit_in_bytes")); got != "67108864" {
		t.Errorf("memsw = %s", got)
	}
	if got := read(t, filepath.Join(root, "pids", "crt", "c1", "pids.max")); got != "32" {
		t.Errorf("pids = %s", got)
	}
	if err := m.Apply(1234); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(cpu, "cgroup.procs")); got != "1234" {
		t.Errorf("procs = %s", got)
	}
	if err := m.Destroy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cpu); !os.IsNotExist(err) {
		t.Errorf("cgroup dir survived destroy")
	}
}

func TestV1TinyQuotaClamped(t *testing.T) {
	root := fakeV1(t)
	m := NewV1(root, "crt", "c")
	m.Create()
	m.Set(Limits{CPU: 0.001})
	if got := read(t, filepath.Join(root, "cpu", "crt", "c", "cpu.cfs_quota_us")); got != "1000" {
		t.Errorf("quota = %s, want clamp to 1000", got)
	}
}

func TestV1Stats(t *testing.T) {
	root := fakeV1(t)
	m := NewV1(root, "crt", "s")
	m.Create()
	w := func(c, f, v string) { os.WriteFile(filepath.Join(root, c, "crt", "s", f), []byte(v), 0o644) }
	w("cpuacct", "cpuacct.usage", "123456789\n")
	w("cpu", "cpu.stat", "nr_periods 10\nnr_throttled 4\nthrottled_time 5000\n")
	w("memory", "memory.usage_in_bytes", "1048576\n")
	w("memory", "memory.max_usage_in_bytes", "2097152\n")
	w("memory", "memory.limit_in_bytes", "9223372036854771712\n")
	w("memory", "memory.failcnt", "3\n")
	w("memory", "memory.oom_control", "oom_kill_disable 0\nunder_oom 0\noom_kill 2\n")
	w("pids", "pids.current", "5\n")
	w("pids", "pids.max", "max\n")
	s, err := m.Stats()
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{CPUUsageNs: 123456789, NrPeriods: 10, NrThrottled: 4, ThrottledNs: 5000,
		MemUsage: 1 << 20, MemMaxUsage: 2 << 20, MemLimit: 0, MemFailcnt: 3, OOMKills: 2, Pids: 5}
	if s != want {
		t.Errorf("stats\n got %+v\nwant %+v", s, want)
	}
}

func TestV2SetAndStats(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpuset cpu io memory pids\n"), 0o644)
	// A plain directory won't get kernel files; seed the parent's
	// controllers file so enableControllers can read it after mkdir.
	parent := filepath.Join(root, "crt")
	os.MkdirAll(parent, 0o755)
	os.WriteFile(filepath.Join(parent, "cgroup.controllers"), []byte("cpu memory pids\n"), 0o644)

	m := NewV2(root, "crt", "c2")
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(root, "cgroup.subtree_control")); got != "+cpu +memory +pids" {
		t.Errorf("root subtree_control = %q", got)
	}
	os.WriteFile(filepath.Join(m.Dir(), "memory.swap.max"), nil, 0o644)
	if err := m.Set(Limits{CPU: 1.5, Memory: 1 << 30, Pids: 100}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(m.Dir(), "cpu.max")); got != "150000 100000" {
		t.Errorf("cpu.max = %q", got)
	}
	if got := read(t, filepath.Join(m.Dir(), "memory.max")); got != "1073741824" {
		t.Errorf("memory.max = %q", got)
	}
	if got := read(t, filepath.Join(m.Dir(), "memory.swap.max")); got != "0" {
		t.Errorf("swap.max = %q", got)
	}
	w := func(f, v string) { os.WriteFile(filepath.Join(m.Dir(), f), []byte(v), 0o644) }
	w("cpu.stat", "usage_usec 2000\nnr_periods 7\nnr_throttled 2\nthrottled_usec 30\n")
	w("memory.current", "4096\n")
	w("memory.max", "max\n")
	w("memory.events", "low 0\nhigh 0\nmax 9\noom 1\noom_kill 1\n")
	w("pids.current", "3\n")
	s, err := m.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUUsageNs != 2_000_000 || s.ThrottledNs != 30_000 || s.NrThrottled != 2 ||
		s.MemUsage != 4096 || s.MemLimit != 0 || s.MemFailcnt != 9 || s.OOMKills != 1 || s.Pids != 3 {
		t.Errorf("unexpected stats %+v", s)
	}
}

func TestParseKV(t *testing.T) {
	kv := parseKV([]byte("a 1\nbad\nb x\nc 3\n"))
	if len(kv) != 2 || kv["a"] != 1 || kv["c"] != 3 {
		t.Errorf("parseKV = %v", kv)
	}
}
