package monitor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baruahs262/container-runtime/internal/cgroup"
)

// fakeSource burns cpuPerCall ns of "CPU" per wall-clock read and fails
// after failAfter calls (0 = never).
type fakeSource struct {
	mu        sync.Mutex
	start     time.Time
	cores     float64
	mem       uint64
	calls     int
	failAfter int
}

func (f *fakeSource) Stats() (cgroup.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failAfter > 0 && f.calls > f.failAfter {
		return cgroup.Stats{}, errors.New("gone")
	}
	el := time.Since(f.start)
	return cgroup.Stats{
		CPUUsageNs:  uint64(float64(el.Nanoseconds()) * f.cores),
		MemUsage:    f.mem,
		MemLimit:    4 * f.mem,
		NrThrottled: uint64(f.calls),
	}, nil
}

func TestRate(t *testing.T) {
	prev := cgroup.Stats{CPUUsageNs: 1e9, NrThrottled: 3}
	cur := cgroup.Stats{CPUUsageNs: 1.5e9, NrThrottled: 5, MemUsage: 50, MemLimit: 200}
	s := Rate(prev, cur, time.Second, time.Now())
	if s.CPUPercent != 50 {
		t.Errorf("cpu = %v, want 50", s.CPUPercent)
	}
	if s.Throttled != 2 || s.MemPercent != 25 {
		t.Errorf("throttled=%d mem%%=%v", s.Throttled, s.MemPercent)
	}
	// Counter reset (new cgroup) must not produce huge values.
	if s := Rate(cur, prev, time.Second, time.Now()); s.CPUPercent != 0 || s.Throttled != 0 {
		t.Errorf("reset produced %+v", s)
	}
}

func TestWatchComputesCPU(t *testing.T) {
	src := &fakeSource{start: time.Now(), cores: 1.5, mem: 1 << 20}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sum Summary
	for s := range Watch(ctx, src, 20*time.Millisecond) {
		sum.Add(s)
		if sum.Samples == 5 {
			cancel()
		}
	}
	if sum.Samples < 5 {
		t.Fatalf("got %d samples", sum.Samples)
	}
	if sum.AvgCPU < 120 || sum.AvgCPU > 180 {
		t.Errorf("avg cpu = %.1f, want ~150", sum.AvgCPU)
	}
	if sum.PeakMem != 1<<20 || sum.Throttled == 0 {
		t.Errorf("summary %+v", sum)
	}
}

func TestWatchStopsWhenSourceFails(t *testing.T) {
	src := &fakeSource{start: time.Now(), cores: 1, failAfter: 3}
	done := make(chan struct{})
	go func() {
		for range Watch(context.Background(), src, 5*time.Millisecond) {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not stop after source error")
	}
}

func TestHubFansIn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := NewHub(ctx, 10*time.Millisecond)
	const n = 20
	for i := 0; i < n; i++ {
		h.Add(string(rune('a'+i)), &fakeSource{start: time.Now(), cores: 0.5})
	}
	h.Add("a", &fakeSource{start: time.Now()}) // duplicate ignored
	seen := map[string]bool{}
	var total atomic.Int64
	timeout := time.After(3 * time.Second)
	for len(seen) < n {
		select {
		case s := <-h.Samples():
			seen[s.ID] = true
			total.Add(1)
		case <-timeout:
			t.Fatalf("only %d/%d containers reported", len(seen), n)
		}
	}
	h.Remove("a")
	h.Close()
	for range h.Samples() {
	}
	if h.Has("b") {
		t.Error("sampler still registered after Close")
	}
}
