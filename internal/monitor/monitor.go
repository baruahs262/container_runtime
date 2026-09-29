// Package monitor samples cgroup accounting on background goroutines and
// turns raw counters into rates: CPU utilisation, memory pressure,
// throttling and OOM kills.
//
// Watch runs one sampler per container. Hub fans many samplers into a
// single stream so one consumer can observe a whole fleet.
package monitor

import (
	"context"
	"sync"
	"time"

	"github.com/baruahs262/container-runtime/internal/cgroup"
)

// Source is anything that can report cgroup stats.
type Source interface {
	Stats() (cgroup.Stats, error)
}

// Sample is one observation, with rates computed against the previous one.
type Sample struct {
	Time       time.Time    `json:"time"`
	CPUPercent float64      `json:"cpu_percent"` // 100 == one full core
	MemBytes   uint64       `json:"mem_bytes"`
	MemLimit   uint64       `json:"mem_limit,omitempty"`
	MemPercent float64      `json:"mem_percent,omitempty"`
	Pids       uint64       `json:"pids"`
	Throttled  uint64       `json:"throttled"` // periods throttled since last sample
	OOMKills   uint64       `json:"oom_kills"` // cumulative
	Raw        cgroup.Stats `json:"-"`
}

// Rate derives a Sample from two consecutive raw readings.
func Rate(prev, cur cgroup.Stats, dt time.Duration, now time.Time) Sample {
	s := Sample{
		Time:     now,
		MemBytes: cur.MemUsage,
		MemLimit: cur.MemLimit,
		Pids:     cur.Pids,
		OOMKills: cur.OOMKills,
		Raw:      cur,
	}
	if dt > 0 && cur.CPUUsageNs >= prev.CPUUsageNs {
		s.CPUPercent = float64(cur.CPUUsageNs-prev.CPUUsageNs) / float64(dt.Nanoseconds()) * 100
	}
	if cur.NrThrottled >= prev.NrThrottled {
		s.Throttled = cur.NrThrottled - prev.NrThrottled
	}
	if cur.MemLimit > 0 {
		s.MemPercent = float64(cur.MemUsage) / float64(cur.MemLimit) * 100
	}
	return s
}

// Watch samples src every interval on its own goroutine until ctx is done
// or src starts failing (the cgroup was removed). The channel is closed
// when sampling stops. Slow consumers drop samples rather than stall the
// sampler, so timing stays accurate.
func Watch(ctx context.Context, src Source, interval time.Duration) <-chan Sample {
	out := make(chan Sample, 16)
	go func() {
		defer close(out)
		prev, err := src.Stats()
		if err != nil {
			return
		}
		last := time.Now()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				cur, err := src.Stats()
				if err != nil {
					return
				}
				s := Rate(prev, cur, now.Sub(last), now)
				prev, last = cur, now
				select {
				case out <- s:
				default:
				}
			}
		}
	}()
	return out
}

// Summary accumulates samples into run-level figures.
type Summary struct {
	Samples   int     `json:"samples"`
	AvgCPU    float64 `json:"avg_cpu_percent"`
	PeakCPU   float64 `json:"peak_cpu_percent"`
	PeakMem   uint64  `json:"peak_mem_bytes"`
	Throttled uint64  `json:"throttled_periods"`
	OOMKills  uint64  `json:"oom_kills"`
	sumCPU    float64
}

// Add folds one sample in.
func (s *Summary) Add(x Sample) {
	s.Samples++
	s.sumCPU += x.CPUPercent
	s.AvgCPU = s.sumCPU / float64(s.Samples)
	if x.CPUPercent > s.PeakCPU {
		s.PeakCPU = x.CPUPercent
	}
	if x.MemBytes > s.PeakMem {
		s.PeakMem = x.MemBytes
	}
	s.Throttled += x.Throttled
	if x.OOMKills > s.OOMKills {
		s.OOMKills = x.OOMKills
	}
}

// Named tags a sample with the container it came from.
type Named struct {
	ID string
	Sample
}

// Hub runs one Watch goroutine per source and merges their samples.
type Hub struct {
	interval time.Duration
	out      chan Named
	wg       sync.WaitGroup
	mu       sync.Mutex
	cancel   map[string]context.CancelFunc
	ctx      context.Context
	closed   bool
}

// NewHub creates a hub whose samplers stop when ctx ends.
func NewHub(ctx context.Context, interval time.Duration) *Hub {
	return &Hub{interval: interval, out: make(chan Named, 64),
		cancel: map[string]context.CancelFunc{}, ctx: ctx}
}

// Samples is the merged stream. It closes after Close once every sampler
// has exited.
func (h *Hub) Samples() <-chan Named { return h.out }

// Add starts sampling src under id. Adding an existing id is a no-op.
func (h *Hub) Add(id string, src Source) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	if _, ok := h.cancel[id]; ok {
		return
	}
	ctx, cancel := context.WithCancel(h.ctx)
	h.cancel[id] = cancel
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer func() {
			h.mu.Lock()
			delete(h.cancel, id)
			h.mu.Unlock()
			cancel()
		}()
		for s := range Watch(ctx, src, h.interval) {
			select {
			case h.out <- Named{ID: id, Sample: s}:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Has reports whether id is being sampled.
func (h *Hub) Has(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.cancel[id]
	return ok
}

// Remove stops sampling id.
func (h *Hub) Remove(id string) {
	h.mu.Lock()
	if c, ok := h.cancel[id]; ok {
		c()
	}
	h.mu.Unlock()
}

// Close stops every sampler and closes Samples once they exit.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	for _, c := range h.cancel {
		c()
	}
	h.mu.Unlock()
	go func() {
		h.wg.Wait()
		close(h.out)
	}()
}
