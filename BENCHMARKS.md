# Benchmarks

Measured with `crt bench` and `go test -bench` on a Firecracker VM:
4 vCPU (amd64), 16 GB RAM, Linux 6.18, cgroup v1, ext4 root. Numbers
vary by host; reproduce with `make bench`.

Command:

```
crt bench -n 300 -parallel 1,4,8,16 -scenarios bare,limits,monitored,netns
```

## What is measured

- **Throughput**: 300 containers per row, pushed through a pool of N
  worker goroutines. Each container runs the full lifecycle: cgroup
  create and configure, clone into new mount/UTS/IPC/PID (and cgroup)
  namespaces, overlay mount, `/proc` `/sys` `/dev` setup, `pivot_root`,
  exec of a static Go binary that exits immediately, wait, cgroup
  teardown and removal of the container directory.
- **Latency** is the wall time of one `Runtime.Run` call. **Startup**
  is the time from `Run` until the workload is released from the sync
  pipe; the remainder is in-namespace setup, the workload itself, and
  cleanup.
- **Host fork/exec** runs the same binary with no isolation, as a floor.
- **CPU enforcement**: 4 busy threads for 2 s under each quota. The
  monitor goroutine samples `cpuacct.usage` every 100 ms; the first
  sample is dropped because it straddles process start-up.
- **Memory enforcement**: allocate and touch 256 MiB under each limit.
  Swap is disabled for the cgroup, so the limit is a hard ceiling.

### Throughput (300 containers per row)

| scenario | workers | containers/s | mean | p50 | p95 | p99 | max | startup p50 | failed |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| host fork/exec (no isolation) | 1 | 623.6 | 1.60ms | 1.54ms | 2.05ms | 2.27ms | 2.51ms | 0.00ms | 0 |
| namespaces + overlay, no limits | 1 | 126.8 | 7.88ms | 7.55ms | 9.94ms | 17.66ms | 22.63ms | 1.12ms | 0 |
| + limits (cpu 0.5, mem 64m, pids 64) | 1 | 121.1 | 8.25ms | 7.69ms | 10.44ms | 21.46ms | 38.63ms | 1.33ms | 0 |
| + limits + monitor goroutine (50ms) | 1 | 119.9 | 8.34ms | 7.91ms | 10.13ms | 18.15ms | 29.89ms | 1.38ms | 0 |
| + private network namespace | 1 | 126.7 | 7.89ms | 7.68ms | 9.87ms | 11.29ms | 27.96ms | 1.47ms | 0 |
| host fork/exec (no isolation) | 4 | 1793.3 | 2.21ms | 2.12ms | 3.36ms | 4.08ms | 4.52ms | 0.00ms | 0 |
| namespaces + overlay, no limits | 4 | 276.2 | 14.41ms | 13.76ms | 20.86ms | 29.73ms | 38.64ms | 2.40ms | 0 |
| + limits (cpu 0.5, mem 64m, pids 64) | 4 | 275.5 | 14.46ms | 13.88ms | 21.56ms | 26.33ms | 28.24ms | 2.58ms | 0 |
| + limits + monitor goroutine (50ms) | 4 | 276.0 | 14.44ms | 13.91ms | 20.46ms | 22.99ms | 28.47ms | 2.52ms | 0 |
| + private network namespace | 4 | 263.8 | 15.11ms | 14.67ms | 20.80ms | 24.96ms | 31.08ms | 2.89ms | 0 |
| host fork/exec (no isolation) | 8 | 1765.3 | 4.45ms | 4.20ms | 7.44ms | 10.25ms | 12.82ms | 0.00ms | 0 |
| namespaces + overlay, no limits | 8 | 309.5 | 25.67ms | 25.29ms | 37.57ms | 42.45ms | 50.48ms | 4.19ms | 0 |
| + limits (cpu 0.5, mem 64m, pids 64) | 8 | 271.3 | 29.14ms | 28.34ms | 45.30ms | 49.32ms | 53.71ms | 5.03ms | 0 |
| + limits + monitor goroutine (50ms) | 8 | 255.9 | 31.06ms | 30.10ms | 45.55ms | 60.45ms | 84.31ms | 5.34ms | 0 |
| + private network namespace | 8 | 243.0 | 32.52ms | 30.25ms | 51.31ms | 71.95ms | 85.33ms | 6.32ms | 0 |
| host fork/exec (no isolation) | 16 | 1941.8 | 7.60ms | 6.51ms | 15.85ms | 20.88ms | 32.29ms | 0.00ms | 0 |
| namespaces + overlay, no limits | 16 | 305.0 | 51.37ms | 49.03ms | 79.52ms | 90.71ms | 114.16ms | 8.60ms | 0 |
| + limits (cpu 0.5, mem 64m, pids 64) | 16 | 306.4 | 51.49ms | 49.30ms | 83.24ms | 100.01ms | 109.05ms | 8.63ms | 0 |
| + limits + monitor goroutine (50ms) | 16 | 284.9 | 55.51ms | 54.26ms | 85.31ms | 106.16ms | 110.81ms | 11.53ms | 0 |
| + private network namespace | 16 | 258.9 | 60.78ms | 58.55ms | 98.06ms | 110.90ms | 120.36ms | 18.28ms | 0 |

### CPU limit enforcement (4 burner threads for 2s, sampled every 100ms)

| quota (cores) | expected | measured avg | measured peak | error | throttled periods |
|---:|---:|---:|---:|---:|---:|
| 0.25 | 25% | 25.1% | 28.8% | +0.5% | 20 |
| 0.5 | 50% | 49.9% | 54.2% | -0.1% | 20 |
| 1 | 100% | 100.0% | 101.6% | -0.0% | 19 |
| 2 | 200% | 200.1% | 203.0% | +0.0% | 19 |

### Memory limit enforcement (allocate and touch 256 MiB)

| limit | peak usage | outcome |
|---:|---:|---|
| 32.0MiB | 32.0MiB | OOM-killed (exit 137) |
| 64.0MiB | 64.0MiB | OOM-killed (exit 137) |
| 128.0MiB | 128.0MiB | OOM-killed (exit 137) |
| 512.0MiB | 258.6MiB | completed (exit 0) |

## Go microbenchmarks

```
go test -run xxx -bench . -benchtime 200x ./internal/container/
BenchmarkRun-4               200   7763600 ns/op   # one full lifecycle, serial
BenchmarkRunParallel-4       200   3278820 ns/op   # amortised, 4 goroutines
BenchmarkCgroupLifecycle-4   200    369925 ns/op   # cgroup mkdir + limits + rmdir
```

## Reading the numbers

- **Throughput saturates near 300 containers/s** from 8 workers up on
  4 vCPUs, about 6x fewer than bare fork/exec. Latency then grows
  linearly with workers, so the extra concurrency is queuing. The
  bottleneck was not profiled. Mount namespace creation and teardown,
  overlay mounts and cgroup directory operations take kernel-wide locks,
  which is the likely limit.
- **Serial cost is about 8 ms per container**, of which about 1.1 ms is
  parent-side startup (cgroup setup, clone, spec hand-off) and about
  0.4 ms is cgroup lifecycle alone.
- **Limits are cheap**: writing three cgroup files cost under 5% at 1, 4
  and 16 workers. The 12% dip at 8 workers did not recur at 16, so it is
  likely run-to-run noise, but it was not re-measured.
- **The monitor goroutine costs 1% serially and up to 7% at 16 workers**
  with a 50 ms sampling interval. Containers here live about 50 ms, so
  that is roughly one sample each; long-lived containers amortise it.
- **A private network namespace costs 15 to 21% at 8 or more workers**
  and nothing serially. Network namespace teardown is known to be
  expensive in the kernel.
- **CPU quotas are accurate to within 0.5%** on average across 0.25 to
  2 cores. Peaks run a few percent over because 100 ms samples do not
  align with the 100 ms CFS period.
- **Memory limits are hard**: every limit below the allocation ended in
  an OOM kill at exactly the limit. Each was reported through the eventfd
  OOM notifier and exit code 137. The 512 MiB run completed at 259 MiB.
