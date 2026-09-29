package container

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const shimArg = "crt-shim"

func (r *Runtime) specPath(id string) string { return filepath.Join(r.Dir(id), "spec.json") }

// Detach starts the container under a background supervisor ("shim")
// that survives this process, and returns once the container is running
// or has failed to start. Output goes to LogPath(id).
func (r *Runtime) Detach(spec *Spec, o RunOpts) (*State, error) {
	if err := r.Prepare(spec); err != nil {
		return nil, err
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(r.specPath(spec.ID), b, 0o644); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(r.LogPath(spec.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	remove := "keep"
	if o.Remove {
		remove = "rm"
	}
	interval := o.MonitorInterval.String()
	cmd := &exec.Cmd{
		Path:        "/proc/self/exe",
		Args:        []string{shimArg, r.Root, r.CgroupMount, r.CgroupParent, spec.ID, remove, interval},
		Stdout:      log,
		Stderr:      log,
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start shim: %w", err)
	}
	shimPid := cmd.Process.Pid
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st, err := r.Load(spec.ID)
		if err == nil && st.ShimPid == shimPid {
			switch st.Status {
			case StatusRunning:
				return st, nil
			case StatusExited, StatusDead:
				if st.Error != "" {
					return st, fmt.Errorf("%s", st.Error)
				}
				return st, nil
			}
		}
		select {
		case <-exited:
			if st, err := r.Load(spec.ID); err == nil && st.Status != StatusCreated {
				return st, nil
			}
			if o.Remove {
				// The shim may have already cleaned up after a quick exit.
				return &State{Spec: spec, Status: StatusExited}, nil
			}
			return nil, fmt.Errorf("shim exited before the container started; see %s", r.LogPath(spec.ID))
		case <-time.After(10 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("timed out waiting for container %s to start", spec.ID)
}

// shimMain supervises a detached container. It never returns.
func shimMain() {
	if len(os.Args) != 7 {
		fmt.Fprintln(os.Stderr, "crt-shim: bad arguments")
		os.Exit(2)
	}
	r := &Runtime{Root: os.Args[1], CgroupMount: os.Args[2], CgroupParent: os.Args[3]}
	id := os.Args[4]
	b, err := os.ReadFile(r.specPath(id))
	if err != nil {
		fmt.Fprintln(os.Stderr, "crt-shim:", err)
		os.Exit(1)
	}
	var spec Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		fmt.Fprintln(os.Stderr, "crt-shim:", err)
		os.Exit(1)
	}
	interval, _ := time.ParseDuration(os.Args[6])
	devnull, _ := os.Open(os.DevNull)
	res, err := r.Run(context.Background(), &spec, RunOpts{
		Stdin:           devnull,
		Stdout:          os.Stdout,
		Stderr:          os.Stderr,
		ForwardSignals:  true,
		Detached:        true,
		Remove:          os.Args[5] == "rm",
		MonitorInterval: interval,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "crt:", err)
		os.Exit(1)
	}
	os.Exit(res.ExitCode)
}
