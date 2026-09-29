package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/baruahs262/container-runtime/internal/cgroup"
	"github.com/baruahs262/container-runtime/internal/monitor"
)

// RunOpts controls how the parent supervises a container.
type RunOpts struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer

	// MonitorInterval > 0 starts a sampling goroutine for the lifetime of
	// the container. Samples feed Result.Summary and OnSample.
	MonitorInterval time.Duration
	OnSample        func(monitor.Sample)

	// OnStart runs once the workload has been released, with its host pid.
	OnStart func(pid int)

	// ForwardSignals relays SIGINT/SIGTERM/SIGHUP/SIGQUIT received by this
	// process to the container; a second signal kills the cgroup.
	ForwardSignals bool

	// Remove deletes the container directory after exit.
	Remove bool

	// Detached marks the state record (set by the shim).
	Detached bool
}

// Result describes a finished container.
type Result struct {
	ID        string          `json:"id"`
	ExitCode  int             `json:"exit_code"`
	Signal    string          `json:"signal,omitempty"`
	OOMKilled bool            `json:"oom_killed"`
	Startup   time.Duration   `json:"startup_ns"` // Run called -> workload released
	Duration  time.Duration   `json:"duration_ns"`
	Summary   monitor.Summary `json:"summary"`
	Final     cgroup.Stats    `json:"final"`
}

// ErrInit wraps failures inside the container before the workload execs
// (bad rootfs, missing binary, mount errors).
var ErrInit = errors.New("container init failed")

const initArg = "crt-init"

// Run creates the container described by spec, runs it to completion and
// cleans up its cgroup. The container directory (with its writable upper
// layer) is kept unless opts.Remove is set.
func (r *Runtime) Run(ctx context.Context, spec *Spec, o RunOpts) (res *Result, err error) {
	t0 := time.Now()
	if err := r.Prepare(spec); err != nil {
		return nil, err
	}
	st := &State{Spec: spec, Status: StatusCreated, Created: t0, Detached: o.Detached,
		ShimPid: os.Getpid()}
	if err := r.Save(st); err != nil {
		return nil, err
	}
	res = &Result{ID: spec.ID}
	defer func() {
		if err != nil {
			st.Error = err.Error()
		}
		if st.Status != StatusExited {
			st.Status = StatusExited
			st.Finished = time.Now()
		}
		_ = r.Save(st)
		if o.Remove {
			_ = os.RemoveAll(r.Dir(spec.ID))
		}
	}()

	mgr, err := r.Cgroup(spec.ID)
	if err != nil {
		return res, err
	}
	if err := mgr.Create(); err != nil {
		return res, err
	}
	defer mgr.Destroy()
	if err := mgr.Set(spec.Limits); err != nil {
		return res, err
	}
	st.Cgroup = mgr.Path()

	// fd 3: spec (parent -> child), fd 4: release (parent closes when the
	// child is in its cgroup), fd 5: init error (child -> parent, CLOEXEC,
	// so it reads EOF once the workload execs).
	specR, specW, err := os.Pipe()
	if err != nil {
		return res, err
	}
	syncR, syncW, err := os.Pipe()
	if err != nil {
		return res, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return res, err
	}
	defer errR.Close()

	cmd := &exec.Cmd{
		Path:       "/proc/self/exe",
		Args:       []string{initArg},
		Env:        []string{},
		Stdin:      o.Stdin,
		Stdout:     o.Stdout,
		Stderr:     o.Stderr,
		ExtraFiles: []*os.File{specR, syncR, errW},
	}
	flags := uintptr(syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC | syscall.CLONE_NEWPID)
	if spec.Network == NetworkNone {
		flags |= syscall.CLONE_NEWNET
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flags, Pdeathsig: syscall.SIGKILL}

	// Pdeathsig fires when the *thread* that forked exits; pin it.
	runtime.LockOSThread()
	err = cmd.Start()
	runtime.UnlockOSThread()
	specR.Close()
	syncR.Close()
	errW.Close()
	if err != nil {
		specW.Close()
		syncW.Close()
		return res, fmt.Errorf("start init: %w", err)
	}
	pid := cmd.Process.Pid
	abort := func(e error) (*Result, error) {
		syncW.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return res, e
	}

	if err := json.NewEncoder(specW).Encode(spec); err != nil {
		specW.Close()
		return abort(fmt.Errorf("send spec: %w", err))
	}
	specW.Close()
	if err := mgr.Apply(pid); err != nil {
		return abort(err)
	}

	// Supervisors: OOM notifications and periodic sampling.
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	var oom atomic.Bool
	var wg sync.WaitGroup
	if spec.Limits.Memory > 0 {
		if ch, err := mgr.OOMEvents(watchCtx); err == nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range ch {
					oom.Store(true)
				}
			}()
		}
	}
	if o.MonitorInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range monitor.Watch(watchCtx, mgr, o.MonitorInterval) {
				res.Summary.Add(s)
				if o.OnSample != nil {
					o.OnSample(s)
				}
			}
		}()
	}

	st.Status, st.Pid, st.Started = StatusRunning, pid, time.Now()
	if err := r.Save(st); err != nil {
		return abort(err)
	}
	errc := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(errR); errc <- b }()
	syncW.Close() // release the child
	res.Startup = time.Since(t0)
	if o.OnStart != nil {
		o.OnStart(pid)
	}

	stopSignals := r.relay(ctx, o.ForwardSignals, pid, mgr)
	waitErr := cmd.Wait()
	stopSignals()
	initErr := <-errc
	res.Duration = time.Since(st.Started)

	if s, err := mgr.Stats(); err == nil {
		res.Final = s
	}
	_ = mgr.Kill() // PID-namespace teardown already did this; be sure
	stopWatch()
	wg.Wait()

	res.ExitCode, res.Signal = exitStatus(cmd.ProcessState, waitErr)
	res.OOMKilled = oom.Load() || res.Final.OOMKills > 0
	if res.OOMKilled && res.Summary.OOMKills == 0 {
		res.Summary.OOMKills = res.Final.OOMKills
	}
	st.Status, st.Finished = StatusExited, time.Now()
	st.ExitCode, st.OOMKilled = res.ExitCode, res.OOMKilled
	if len(initErr) > 0 {
		return res, fmt.Errorf("%w: %s", ErrInit, initErr)
	}
	return res, nil
}

// relay forwards termination signals and ctx cancellation to the
// container. It returns a function that stops relaying.
func (r *Runtime) relay(ctx context.Context, forward bool, pid int, mgr cgroup.Manager) func() {
	done := make(chan struct{})
	sigc := make(chan os.Signal, 4)
	if forward {
		signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	}
	go func() {
		n := 0
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				_ = mgr.Kill()
				return
			case s := <-sigc:
				n++
				// PID 1 of a namespace ignores signals it has no handler
				// for, so escalate if the first one does not do the job.
				if n == 1 {
					_ = syscall.Kill(pid, s.(syscall.Signal))
					go func() {
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							_ = mgr.Kill()
						}
					}()
				} else {
					_ = mgr.Kill()
				}
			}
		}
	}()
	return func() {
		if forward {
			signal.Stop(sigc)
		}
		close(done)
	}
}

// exitStatus maps a process state to a shell-style exit code.
func exitStatus(ps *os.ProcessState, err error) (int, string) {
	if ps == nil {
		if err != nil {
			return 255, ""
		}
		return 0, ""
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), ws.Signal().String()
	}
	return ps.ExitCode(), ""
}
