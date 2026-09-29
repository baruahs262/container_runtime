// Package bench measures container throughput and resource-limit
// accuracy.
//
// Throughput runs N short-lived containers through a pool of P worker
// goroutines and reports containers per second and latency percentiles.
// Enforcement runs a CPU burner under several quotas and compares the
// utilisation the monitor observed with the limit that was set.
package bench

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/baruahs262/container-runtime/internal/cgroup"
	"github.com/baruahs262/container-runtime/internal/container"
	"github.com/baruahs262/container-runtime/internal/monitor"
)

// Scenario is one throughput configuration.
type Scenario struct {
	Name            string
	Limits          cgroup.Limits
	MonitorInterval time.Duration
	Network         string
}

// Config drives a throughput run.
type Config struct {
	Runtime  *container.Runtime
	Layers   []string
	LayerIDs []string
	Args     []string
	N        int
	Parallel int
	Scenario Scenario
}

// Result summarises a throughput run.
type Result struct {
	Name        string
	N, Parallel int
	Failures    int
	Wall        time.Duration
	Throughput  float64 // containers per second
	Mean        time.Duration
	P50         time.Duration
	P95         time.Duration
	P99         time.Duration
	Max         time.Duration
	StartupP50  time.Duration // Run() call -> workload released
	FirstError  string
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p*float64(len(sorted)-1) + 0.5)
	return sorted[i]
}

func summarise(name string, n, par int, wall time.Duration, lat, startup []time.Duration, fails int, firstErr string) Result {
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	sort.Slice(startup, func(i, j int) bool { return startup[i] < startup[j] })
	var sum time.Duration
	for _, l := range lat {
		sum += l
	}
	r := Result{Name: name, N: n, Parallel: par, Failures: fails, Wall: wall, FirstError: firstErr}
	if len(lat) > 0 {
		r.Mean = sum / time.Duration(len(lat))
		r.P50, r.P95, r.P99 = percentile(lat, .50), percentile(lat, .95), percentile(lat, .99)
		r.Max = lat[len(lat)-1]
	}
	r.StartupP50 = percentile(startup, .50)
	r.Throughput = float64(len(lat)) / wall.Seconds()
	return r
}

// pool runs n jobs on par goroutines and gathers per-job latencies.
func pool(ctx context.Context, n, par int, job func() (lat, startup time.Duration, err error)) (wall time.Duration, lats, starts []time.Duration, fails int, firstErr string) {
	var mu sync.Mutex
	var next atomic.Int64
	var wg sync.WaitGroup
	t0 := time.Now()
	for w := 0; w < par; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for next.Add(1) <= int64(n) && ctx.Err() == nil {
				l, s, err := job()
				mu.Lock()
				if err != nil {
					fails++
					if firstErr == "" {
						firstErr = err.Error()
					}
				} else {
					lats = append(lats, l)
					starts = append(starts, s)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return time.Since(t0), lats, starts, fails, firstErr
}

// Throughput launches cfg.N containers, cfg.Parallel at a time. Each
// container is created, run to completion and removed.
func Throughput(ctx context.Context, cfg Config) Result {
	wall, lats, starts, fails, firstErr := pool(ctx, cfg.N, cfg.Parallel, func() (time.Duration, time.Duration, error) {
		spec := &container.Spec{
			Image:    "bench",
			Layers:   cfg.Layers,
			LayerIDs: cfg.LayerIDs,
			Args:     cfg.Args,
			Limits:   cfg.Scenario.Limits,
			Network:  cfg.Scenario.Network,
		}
		t0 := time.Now()
		res, err := cfg.Runtime.Run(ctx, spec, container.RunOpts{
			Stdout: io.Discard, Stderr: io.Discard, Remove: true,
			MonitorInterval: cfg.Scenario.MonitorInterval,
		})
		if err != nil {
			return 0, 0, err
		}
		if res.ExitCode != 0 {
			return 0, 0, fmt.Errorf("container exited %d", res.ExitCode)
		}
		return time.Since(t0), res.Startup, nil
	})
	return summarise(cfg.Scenario.Name, cfg.N, cfg.Parallel, wall, lats, starts, fails, firstErr)
}

// HostExec is the no-isolation reference: plain fork/exec of the same
// binary on the host.
func HostExec(ctx context.Context, path string, args []string, n, par int) Result {
	wall, lats, starts, fails, firstErr := pool(ctx, n, par, func() (time.Duration, time.Duration, error) {
		t0 := time.Now()
		err := exec.CommandContext(ctx, path, args...).Run()
		return time.Since(t0), 0, err
	})
	return summarise("host fork/exec (no isolation)", n, par, wall, lats, starts, fails, firstErr)
}

// EnforcementResult compares a CPU quota with measured usage.
type EnforcementResult struct {
	LimitCores  float64
	AvgCPU      float64 // percent of one core, from the monitor
	PeakCPU     float64
	ErrorPct    float64 // (measured - limit) / limit
	Throttled   uint64
	Samples     int
	ExitCode    int
	MemLimit    int64
	MemPeak     uint64
	OOMKilled   bool
	Description string
}

// CPUEnforcement burns all cores for d under each quota and reports what
// the monitoring goroutine measured.
func CPUEnforcement(ctx context.Context, rt *container.Runtime, layers []string, stress string, threads int, d time.Duration, quotas []float64) ([]EnforcementResult, error) {
	var out []EnforcementResult
	for _, q := range quotas {
		spec := &container.Spec{
			Image:  "bench",
			Layers: layers,
			Args:   []string{stress, "-cpu", d.String(), "-threads", fmt.Sprint(threads)},
			Limits: cgroup.Limits{CPU: q},
		}
		var sum monitor.Summary
		// Skip the first sample: it straddles process start-up.
		first := true
		res, err := rt.Run(ctx, spec, container.RunOpts{
			Stdout: io.Discard, Stderr: io.Discard, Remove: true,
			MonitorInterval: 100 * time.Millisecond,
			OnSample: func(s monitor.Sample) {
				if first {
					first = false
					return
				}
				sum.Add(s)
			},
		})
		if err != nil {
			return out, err
		}
		limitPct := q * 100
		out = append(out, EnforcementResult{
			LimitCores: q, AvgCPU: sum.AvgCPU, PeakCPU: sum.PeakCPU,
			ErrorPct:  (sum.AvgCPU - limitPct) / limitPct * 100,
			Throttled: res.Summary.Throttled, Samples: sum.Samples, ExitCode: res.ExitCode,
		})
	}
	return out, nil
}

// MemoryEnforcement allocates allocMiB under each memory limit and
// reports whether the kernel held the line.
func MemoryEnforcement(ctx context.Context, rt *container.Runtime, layers []string, stress string, allocMiB int, limits []int64) ([]EnforcementResult, error) {
	var out []EnforcementResult
	for _, l := range limits {
		spec := &container.Spec{
			Image:  "bench",
			Layers: layers,
			Args:   []string{stress, "-mem", fmt.Sprint(allocMiB), "-hold", "300ms"},
			Limits: cgroup.Limits{Memory: l},
		}
		res, err := rt.Run(ctx, spec, container.RunOpts{
			Stdout: io.Discard, Stderr: io.Discard, Remove: true,
			MonitorInterval: 20 * time.Millisecond,
		})
		if err != nil {
			return out, err
		}
		peak := res.Final.MemMaxUsage
		if peak == 0 {
			peak = res.Summary.PeakMem
		}
		out = append(out, EnforcementResult{
			MemLimit: l, MemPeak: peak, OOMKilled: res.OOMKilled, ExitCode: res.ExitCode,
			Description: fmt.Sprintf("alloc %d MiB", allocMiB),
		})
	}
	return out, nil
}
