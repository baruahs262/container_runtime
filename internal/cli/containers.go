package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/baruahs262/container-runtime/internal/cgroup"
	"github.com/baruahs262/container-runtime/internal/container"
	"github.com/baruahs262/container-runtime/internal/image"
	"github.com/baruahs262/container-runtime/internal/monitor"
	"github.com/baruahs262/container-runtime/internal/rootfs"
)

func (a *App) cmdRun(args []string) error {
	fs := a.newFlags("run", "run [OPTIONS] IMAGE [COMMAND [ARG...]]")
	cpu := fs.Float64("cpu", 0, "CPU limit in cores (e.g. 0.5)")
	mem := fs.String("memory", "", "memory limit (e.g. 64m, 1g); swap is disabled")
	pids := fs.Int64("pids", 0, "maximum number of processes")
	hostname := fs.String("hostname", "", "container hostname (default: container id)")
	network := fs.String("network", container.NetworkHost, "host | none (private namespace, loopback only)")
	isolation := fs.String("isolation", container.IsolationPivot, "pivot (pivot_root) | chroot")
	workdir := fs.String("w", "", "working directory inside the container")
	entrypoint := fs.String("entrypoint", "", "override the image entrypoint")
	detach := fs.Bool("d", false, "run in the background and print the container id")
	remove := fs.Bool("rm", false, "remove the container when it exits")
	stats := fs.Bool("stats", false, "monitor resource usage and print a summary on exit")
	interval := fs.Duration("stats-interval", 250*time.Millisecond, "monitor sampling interval")
	init := fs.Bool("init", false, "run a minimal init as PID 1 that reaps zombies and forwards signals")
	privileged := fs.Bool("privileged", false, "keep all capabilities")
	readOnly := fs.Bool("read-only", false, "mount the root filesystem read-only")
	var env, volumes multiFlag
	fs.Var(&env, "e", "set an environment variable KEY=VALUE (repeatable)")
	fs.Var(&volumes, "v", "bind mount HOST:CONTAINER[:ro] (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return ExitError{2}
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	img, err := st.Get(fs.Arg(0))
	if err != nil {
		return err
	}
	dirs, err := st.Dirs(img)
	if err != nil {
		return err
	}
	ep := img.Config.Entrypoint
	if *entrypoint != "" {
		ep = []string{*entrypoint}
	}
	cmd := img.Config.Cmd
	if fs.NArg() > 1 {
		cmd = fs.Args()[1:]
	}
	argv := append(append([]string{}, ep...), cmd...)
	if len(argv) == 0 {
		return fmt.Errorf("image %s has no default command; pass one", img.Ref())
	}
	limits := cgroup.Limits{CPU: *cpu, Pids: *pids}
	if *mem != "" {
		if limits.Memory, err = ParseBytes(*mem); err != nil {
			return err
		}
	}
	binds, err := parseVolumes(volumes)
	if err != nil {
		return err
	}
	wd := img.Config.WorkingDir
	if *workdir != "" {
		wd = *workdir
	}
	spec := &container.Spec{
		Image: img.Ref(), Layers: dirs, LayerIDs: img.Layers, Args: argv,
		Env: mergeEnv(img.Config.Env, env), WorkDir: wd, Hostname: *hostname,
		Limits: limits, Isolation: *isolation, Network: *network, Binds: binds,
		Init: *init, Privileged: *privileged, ReadOnly: *readOnly,
	}
	rt := a.runtime()
	if *detach {
		opts := container.RunOpts{Remove: *remove}
		if *stats {
			opts.MonitorInterval = *interval
		}
		s, err := rt.Detach(spec, opts)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.Stdout, s.Spec.ID)
		return nil
	}
	opts := container.RunOpts{
		Stdin: os.Stdin, Stdout: a.Stdout, Stderr: a.Stderr,
		ForwardSignals: true, Remove: *remove,
	}
	if *stats {
		opts.MonitorInterval = *interval
	}
	res, err := rt.Run(context.Background(), spec, opts)
	if err != nil {
		if errors.Is(err, container.ErrInit) && res != nil {
			fmt.Fprintln(a.Stderr, "crt:", err)
			return ExitError{res.ExitCode}
		}
		return err
	}
	if *stats {
		a.printSummary(res, limits)
	}
	if res.OOMKilled {
		fmt.Fprintf(a.Stderr, "crt: container %s was killed: out of memory (limit %s)\n", res.ID, HumanBytes(uint64(limits.Memory)))
	}
	if res.ExitCode != 0 {
		return ExitError{res.ExitCode}
	}
	return nil
}

func (a *App) printSummary(res *container.Result, l cgroup.Limits) {
	s := res.Summary
	w := a.Stderr
	fmt.Fprintf(w, "--- %s: exit %d in %s (startup %s)\n", res.ID, res.ExitCode, res.Duration.Round(time.Millisecond), res.Startup.Round(time.Microsecond))
	cpuLim, memLim := "unlimited", "unlimited"
	if l.CPU > 0 {
		cpuLim = fmt.Sprintf("%.0f%%", l.CPU*100)
	}
	if l.Memory > 0 {
		memLim = HumanBytes(uint64(l.Memory))
	}
	fmt.Fprintf(w, "    cpu    avg %.1f%%  peak %.1f%%  limit %s  throttled %d periods  total %s\n",
		s.AvgCPU, s.PeakCPU, cpuLim, s.Throttled, time.Duration(res.Final.CPUUsageNs).Round(time.Millisecond))
	peak := res.Final.MemMaxUsage
	if peak < s.PeakMem {
		peak = s.PeakMem
	}
	fmt.Fprintf(w, "    memory peak %s  limit %s  oom kills %d\n", HumanBytes(peak), memLim, res.Final.OOMKills)
	fmt.Fprintf(w, "    samples %d\n", s.Samples)
}

func parseVolumes(vs []string) ([]rootfs.Bind, error) {
	var out []rootfs.Bind
	for _, v := range vs {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || !strings.HasPrefix(parts[1], "/") {
			return nil, fmt.Errorf("bad volume %q (want /host:/container[:ro])", v)
		}
		b := rootfs.Bind{Source: parts[0], Target: parts[1]}
		if len(parts) == 3 {
			if parts[2] != "ro" && parts[2] != "rw" {
				return nil, fmt.Errorf("bad volume mode %q", parts[2])
			}
			b.ReadOnly = parts[2] == "ro"
		}
		if _, err := os.Stat(b.Source); err != nil {
			return nil, fmt.Errorf("volume source: %w", err)
		}
		out = append(out, b)
	}
	return out, nil
}

func mergeEnv(base []string, extra []string) []string {
	out := append([]string{}, base...)
	for _, kv := range extra {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			kv = k + "=" + os.Getenv(k) // "-e VAR" passes the host value
		}
		replaced := false
		for i, e := range out {
			if strings.HasPrefix(e, k+"=") {
				out[i], replaced = kv, true
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out
}

func (a *App) cmdPs(args []string) error {
	fs := a.newFlags("ps", "ps [-a] [-q]")
	all := fs.Bool("a", false, "show all containers (default: running only)")
	quiet := fs.Bool("q", false, "only print ids")
	if err := fs.Parse(args); err != nil {
		return err
	}
	list, err := a.runtime().List()
	if err != nil {
		return err
	}
	tw := a.table()
	if !*quiet {
		fmt.Fprintln(tw, "CONTAINER ID\tIMAGE\tCOMMAND\tCREATED\tSTATUS\tLIMITS")
	}
	for _, st := range list {
		if !*all && st.Status != container.StatusRunning {
			continue
		}
		if *quiet {
			fmt.Fprintln(tw, st.Spec.ID)
			continue
		}
		status := st.Status
		switch st.Status {
		case container.StatusRunning:
			status = "Up " + strings.TrimSuffix(ago(st.Started), " ago")
		case container.StatusExited:
			status = fmt.Sprintf("Exited (%d) %s", st.ExitCode, ago(st.Finished))
			if st.OOMKilled {
				status += " OOM"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%q\t%s\t%s\t%s\n", st.Spec.ID, st.Spec.Image,
			truncate(strings.Join(st.Spec.Args, " "), 30), ago(st.Created), status, fmtLimits(st.Spec.Limits))
	}
	return tw.Flush()
}

func fmtLimits(l cgroup.Limits) string {
	var p []string
	if l.CPU > 0 {
		p = append(p, fmt.Sprintf("cpu=%g", l.CPU))
	}
	if l.Memory > 0 {
		p = append(p, "mem="+HumanBytes(uint64(l.Memory)))
	}
	if l.Pids > 0 {
		p = append(p, fmt.Sprintf("pids=%d", l.Pids))
	}
	if len(p) == 0 {
		return "-"
	}
	return strings.Join(p, ",")
}

func (a *App) cmdStop(args []string) error {
	fs := a.newFlags("stop", "stop [-t SECONDS] CONTAINER...")
	t := fs.Int("t", 10, "seconds to wait before SIGKILL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return a.each(fs.Args(), func(id string) error {
		return a.runtime().Stop(id, time.Duration(*t)*time.Second)
	})
}

var signals = map[string]syscall.Signal{
	"KILL": syscall.SIGKILL, "TERM": syscall.SIGTERM, "INT": syscall.SIGINT, "HUP": syscall.SIGHUP,
	"QUIT": syscall.SIGQUIT, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "STOP": syscall.SIGSTOP, "CONT": syscall.SIGCONT,
}

func (a *App) cmdKill(args []string) error {
	fs := a.newFlags("kill", "kill [-s SIGNAL] CONTAINER...")
	name := fs.String("s", "KILL", "signal name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sig, ok := signals[strings.TrimPrefix(strings.ToUpper(*name), "SIG")]
	if !ok {
		return fmt.Errorf("unknown signal %q", *name)
	}
	return a.each(fs.Args(), func(id string) error { return a.runtime().Signal(id, sig) })
}

func (a *App) cmdRm(args []string) error {
	fs := a.newFlags("rm", "rm [-f] CONTAINER...")
	force := fs.Bool("f", false, "kill running containers first")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return a.each(fs.Args(), func(id string) error { return a.runtime().Remove(id, *force) })
}

func (a *App) cmdPrune(args []string) error {
	gone, err := a.runtime().Prune()
	for _, id := range gone {
		fmt.Fprintln(a.Stdout, id)
	}
	return err
}

// each applies fn to every id, printing ids that succeed.
func (a *App) each(ids []string, fn func(string) error) error {
	if len(ids) == 0 {
		return errors.New("no container given")
	}
	var failed bool
	for _, id := range ids {
		if err := fn(id); err != nil {
			fmt.Fprintln(a.Stderr, "crt:", err)
			failed = true
			continue
		}
		fmt.Fprintln(a.Stdout, id)
	}
	if failed {
		return ExitError{1}
	}
	return nil
}

func (a *App) cmdLogs(args []string) error {
	fs := a.newFlags("logs", "logs [-f] CONTAINER")
	follow := fs.Bool("f", false, "follow output until the container exits")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return ExitError{2}
	}
	rt := a.runtime()
	st, err := rt.Resolve(fs.Arg(0))
	if err != nil {
		return err
	}
	f, err := os.Open(rt.LogPath(st.Spec.ID))
	if err != nil {
		return fmt.Errorf("no logs for %s (only detached containers are logged)", st.Spec.ID)
	}
	defer f.Close()
	for {
		if _, err := io.Copy(a.Stdout, f); err != nil {
			return err
		}
		if !*follow {
			return nil
		}
		cur, err := rt.Load(st.Spec.ID)
		if err != nil || cur.Status != container.StatusRunning {
			_, err := io.Copy(a.Stdout, f)
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (a *App) cmdInspect(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: crt inspect CONTAINER")
	}
	st, err := a.runtime().Resolve(args[0])
	if err != nil {
		return err
	}
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(st)
}

func (a *App) cmdCommit(args []string) error {
	fs := a.newFlags("commit", "commit [-m MESSAGE] CONTAINER IMAGE[:TAG]")
	msg := fs.String("m", "", "commit message")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return ExitError{2}
	}
	rt := a.runtime()
	cst, err := rt.Resolve(fs.Arg(0))
	if err != nil {
		return err
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	var base *image.Image
	if len(cst.Spec.LayerIDs) > 0 {
		base = &image.Image{Layers: cst.Spec.LayerIDs}
		if b, err := st.Get(cst.Spec.Image); err == nil && equalStrings(b.Layers, cst.Spec.LayerIDs) {
			base = b
		}
	}
	comment := *msg
	if comment == "" {
		comment = strings.Join(cst.Spec.Args, " ")
	}
	img, err := st.Commit(base, cst.Spec.Upper, fs.Arg(1), comment)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, img.ID)
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (a *App) cmdStats(args []string) error {
	fs := a.newFlags("stats", "stats [--no-stream] [CONTAINER...]")
	noStream := fs.Bool("no-stream", false, "print one snapshot and exit")
	interval := fs.Duration("interval", time.Second, "refresh interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rt := a.runtime()
	want := map[string]bool{}
	for _, ref := range fs.Args() {
		st, err := rt.Resolve(ref)
		if err != nil {
			return err
		}
		want[st.Spec.ID] = true
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sampleEvery := *interval
	if *noStream {
		sampleEvery = 500 * time.Millisecond
	}
	hub := monitor.NewHub(ctx, sampleEvery)
	defer hub.Close()
	limits := map[string]cgroup.Limits{}
	discover := func() int {
		list, _ := rt.List()
		n := 0
		for _, st := range list {
			if st.Status != container.StatusRunning || (len(want) > 0 && !want[st.Spec.ID]) {
				continue
			}
			n++
			if hub.Has(st.Spec.ID) {
				continue
			}
			mgr, err := rt.Cgroup(st.Spec.ID)
			if err != nil {
				continue
			}
			limits[st.Spec.ID] = st.Spec.Limits
			hub.Add(st.Spec.ID, mgr)
		}
		return n
	}
	expected := discover()
	if expected == 0 {
		return errors.New("no running containers")
	}
	latest := map[string]monitor.Sample{}
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	print := func() {
		ids := make([]string, 0, len(latest))
		for id := range latest {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if !*noStream {
			fmt.Fprint(a.Stdout, "\033[H\033[2J")
		}
		tw := a.table()
		fmt.Fprintln(tw, "CONTAINER ID\tCPU %\tCPU LIMIT\tMEM USAGE / LIMIT\tMEM %\tPIDS\tTHROTTLED\tOOM")
		for _, id := range ids {
			s := latest[id]
			l := limits[id]
			cpuLim, memLim := "-", "-"
			if l.CPU > 0 {
				cpuLim = fmt.Sprintf("%.0f%%", l.CPU*100)
			}
			if s.MemLimit > 0 {
				memLim = HumanBytes(s.MemLimit)
			}
			fmt.Fprintf(tw, "%s\t%.2f%%\t%s\t%s / %s\t%.1f%%\t%d\t%d\t%d\n", id, s.CPUPercent, cpuLim,
				HumanBytes(s.MemBytes), memLim, s.MemPercent, s.Pids, s.Throttled, s.OOMKills)
		}
		tw.Flush()
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case s, ok := <-hub.Samples():
			if !ok {
				return nil
			}
			latest[s.ID] = s.Sample
			if *noStream && len(latest) >= expected {
				print()
				return nil
			}
		case <-tick.C:
			if *noStream {
				continue
			}
			if discover() == 0 {
				print()
				return nil
			}
			for id := range latest {
				if !hub.Has(id) {
					delete(latest, id)
				}
			}
			print()
		case <-deadline:
			if *noStream {
				print()
				return nil
			}
		}
	}
}
