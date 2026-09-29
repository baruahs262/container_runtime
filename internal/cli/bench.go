package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/baruahs262/container-runtime/internal/bench"
	"github.com/baruahs262/container-runtime/internal/cgroup"
	"github.com/baruahs262/container-runtime/internal/image"
)

const benchImage = "crt-bench:latest"

func (a *App) cmdBench(args []string) error {
	fs := a.newFlags("bench", "bench [OPTIONS]")
	n := fs.Int("n", 200, "containers per throughput run")
	parallel := fs.String("parallel", "1,4,8,16", "comma-separated worker counts")
	scenarios := fs.String("scenarios", "bare,limits,monitored", "throughput scenarios to run")
	cpu := fs.Float64("cpu", 0.5, "CPU limit for the limits/monitored scenarios")
	mem := fs.String("memory", "64m", "memory limit for the limits/monitored scenarios")
	interval := fs.Duration("monitor-interval", 50*time.Millisecond, "sampling interval in the monitored scenario")
	stressBin := fs.String("stress-bin", "", "crt-stress binary used to create the bench image (default: next to crt)")
	host := fs.Bool("host-baseline", true, "also time plain fork/exec on the host")
	enforce := fs.Bool("enforcement", true, "measure CPU and memory limit accuracy")
	burn := fs.Duration("burn", 2*time.Second, "CPU burn per enforcement run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var pars []int
	for _, p := range strings.Split(*parallel, ",") {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || v < 1 {
			return fmt.Errorf("bad -parallel value %q", p)
		}
		pars = append(pars, v)
	}
	memLimit, err := ParseBytes(*mem)
	if err != nil {
		return err
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	bin := *stressBin
	if bin == "" {
		exe, _ := os.Executable()
		bin = filepath.Join(filepath.Dir(exe), "crt-stress")
	}
	img, err := a.benchImage(st, bin)
	if err != nil {
		return err
	}
	dirs, err := st.Dirs(img)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rt := a.runtime()
	out := a.Stdout

	fmt.Fprintf(out, "## crt benchmark\n\n")
	fmt.Fprintf(out, "- host: %s, %d CPUs, kernel %s, cgroup v%d\n", runtime.GOARCH, runtime.NumCPU(), kernel(), cgroupVersion())
	fmt.Fprintf(out, "- workload: `/bin/crt-stress -exit 0` (static Go binary, scratch image)\n")
	fmt.Fprintf(out, "- each container: new mnt/uts/ipc/pid/cgroup namespaces, overlay rootfs, pivot_root, cgroup create+destroy, directory cleanup\n\n")

	all := map[string]bench.Scenario{
		"bare":      {Name: "namespaces + overlay, no limits"},
		"limits":    {Name: fmt.Sprintf("+ limits (cpu %g, mem %s, pids 64)", *cpu, *mem), Limits: cgroup.Limits{CPU: *cpu, Memory: memLimit, Pids: 64}},
		"monitored": {Name: fmt.Sprintf("+ limits + monitor goroutine (%s)", *interval), Limits: cgroup.Limits{CPU: *cpu, Memory: memLimit, Pids: 64}, MonitorInterval: *interval},
		"netns":     {Name: "+ private network namespace", Network: "none"},
	}
	fmt.Fprintf(out, "### Throughput (%d containers per row)\n\n", *n)
	fmt.Fprintln(out, "| scenario | workers | containers/s | mean | p50 | p95 | p99 | max | startup p50 | failed |")
	fmt.Fprintln(out, "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	row := func(r bench.Result) {
		fmt.Fprintf(out, "| %s | %d | %.1f | %s | %s | %s | %s | %s | %s | %d |\n", r.Name, r.Parallel, r.Throughput,
			ms(r.Mean), ms(r.P50), ms(r.P95), ms(r.P99), ms(r.Max), ms(r.StartupP50), r.Failures)
		if r.FirstError != "" {
			fmt.Fprintf(a.Stderr, "  first error: %s\n", r.FirstError)
		}
	}
	for _, p := range pars {
		if *host {
			row(bench.HostExec(ctx, bin, []string{"-exit", "0"}, *n, p))
		}
		for _, name := range strings.Split(*scenarios, ",") {
			sc, ok := all[strings.TrimSpace(name)]
			if !ok {
				return fmt.Errorf("unknown scenario %q", name)
			}
			row(bench.Throughput(ctx, bench.Config{
				Runtime: rt, Layers: dirs, LayerIDs: img.Layers,
				Args: []string{"/bin/crt-stress", "-exit", "0"}, N: *n, Parallel: p, Scenario: sc,
			}))
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	if !*enforce {
		return nil
	}

	threads := runtime.NumCPU()
	fmt.Fprintf(out, "\n### CPU limit enforcement (%d burner threads for %s, sampled every 100ms)\n\n", threads, *burn)
	fmt.Fprintln(out, "| quota (cores) | expected | measured avg | measured peak | error | throttled periods |")
	fmt.Fprintln(out, "|---:|---:|---:|---:|---:|---:|")
	var quotas []float64
	for _, q := range []float64{0.25, 0.5, 1, 2} {
		if q <= float64(runtime.NumCPU()) {
			quotas = append(quotas, q)
		}
	}
	cres, err := bench.CPUEnforcement(ctx, rt, dirs, "/bin/crt-stress", threads, *burn, quotas)
	if err != nil {
		return err
	}
	for _, r := range cres {
		fmt.Fprintf(out, "| %g | %.0f%% | %.1f%% | %.1f%% | %+.1f%% | %d |\n", r.LimitCores, r.LimitCores*100, r.AvgCPU, r.PeakCPU, r.ErrorPct, r.Throttled)
	}

	fmt.Fprintf(out, "\n### Memory limit enforcement (allocate and touch 256 MiB)\n\n")
	fmt.Fprintln(out, "| limit | peak usage | outcome |")
	fmt.Fprintln(out, "|---:|---:|---|")
	mres, err := bench.MemoryEnforcement(ctx, rt, dirs, "/bin/crt-stress", 256, []int64{32 << 20, 64 << 20, 128 << 20, 512 << 20})
	if err != nil {
		return err
	}
	for _, r := range mres {
		outcome := fmt.Sprintf("completed (exit %d)", r.ExitCode)
		if r.OOMKilled {
			outcome = fmt.Sprintf("OOM-killed (exit %d)", r.ExitCode)
		}
		fmt.Fprintf(out, "| %s | %s | %s |\n", HumanBytes(uint64(r.MemLimit)), HumanBytes(r.MemPeak), outcome)
	}
	return nil
}

// benchImage returns crt-bench:latest, creating it from bin if needed.
func (a *App) benchImage(st *image.Store, bin string) (*image.Image, error) {
	if img, err := st.Get(benchImage); err == nil {
		return img, nil
	}
	f, err := os.Open(bin)
	if err != nil {
		return nil, fmt.Errorf("bench image %s missing and no crt-stress binary at %s (build it with `make`, or pass -stress-bin)", benchImage, bin)
	}
	defer f.Close()
	fi, _ := f.Stat()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0)})
	tw.WriteHeader(&tar.Header{Name: "tmp/", Typeflag: tar.TypeDir, Mode: 0o1777, ModTime: time.Unix(0, 0)})
	tw.WriteHeader(&tar.Header{Name: "bin/crt-stress", Typeflag: tar.TypeReg, Mode: 0o755, Size: fi.Size(), ModTime: time.Unix(0, 0)})
	if _, err := io.Copy(tw, f); err != nil {
		return nil, err
	}
	tw.Close()
	fmt.Fprintf(a.Stderr, "created %s from %s\n", benchImage, bin)
	return st.ImportRootfs(&buf, benchImage, image.Config{Cmd: []string{"/bin/crt-stress"}})
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000)
}

func kernel() string {
	var u syscall.Utsname
	if syscall.Uname(&u) != nil {
		return "?"
	}
	var b []byte
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

func cgroupVersion() int {
	if cgroup.IsV2(cgroup.DefaultMount) {
		return 2
	}
	return 1
}
