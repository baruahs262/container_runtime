package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"github.com/baruahs262/container-runtime/internal/rootfs"
)

// Exit codes used by init before the workload runs, mirroring docker.
const (
	ExitInitFailed  = 125
	ExitNotRunnable = 126
	ExitNotFound    = 127
)

// The runtime re-executes its own binary for two roles. Detecting them in
// a package initialiser means every program that links this package
// (the crt CLI, tests, benchmarks) can spawn containers without extra
// wiring in main.
func init() {
	if len(os.Args) == 0 {
		return
	}
	switch os.Args[0] {
	case initArg:
		// Namespace-affecting syscalls such as unshare act on the calling
		// thread, which must also be the one that execs.
		runtime.LockOSThread()
		containerInit()
	case shimArg:
		shimMain()
	}
}

// containerInit is PID 1 of the new PID namespace. It never returns.
func containerInit() {
	errPipe := os.NewFile(5, "init-error")
	syscall.CloseOnExec(5)
	fail := func(code int, err error) {
		fmt.Fprint(errPipe, err.Error())
		os.Exit(code)
	}

	var spec Spec
	specPipe := os.NewFile(3, "spec")
	if err := json.NewDecoder(specPipe).Decode(&spec); err != nil {
		fail(ExitInitFailed, fmt.Errorf("read spec: %w", err))
	}
	specPipe.Close()

	// Block until the parent has placed us in the cgroup.
	syncPipe := os.NewFile(4, "sync")
	if _, err := io.Copy(io.Discard, syncPipe); err != nil {
		fail(ExitInitFailed, fmt.Errorf("sync: %w", err))
	}
	syncPipe.Close()

	if err := setup(&spec); err != nil {
		fail(ExitInitFailed, err)
	}

	env := spec.Env
	if !hasEnv(env, "PATH") {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if !hasEnv(env, "HOSTNAME") {
		env = append(env, "HOSTNAME="+spec.Hostname)
	}
	os.Clearenv()
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			os.Setenv(k, v)
		}
	}
	path, err := lookPath(spec.Args[0])
	if err != nil {
		fail(ExitNotFound, err)
	}
	if !spec.Privileged {
		if err := dropCapabilities(); err != nil {
			fail(ExitInitFailed, err)
		}
	}
	if spec.Init {
		os.Exit(reaper(path, spec.Args, env, errPipe))
	}
	err = syscall.Exec(path, spec.Args, env)
	fail(ExitNotRunnable, fmt.Errorf("exec %s: %w", path, err))
}

// setup builds the filesystem and identity of the container. It runs in
// the fresh mount/UTS/IPC/PID namespaces created by clone.
func setup(s *Spec) error {
	// Join a new cgroup namespace now that we are in our cgroup, so the
	// container sees its own cgroup as "/".
	if err := syscall.Unshare(syscall.CLONE_NEWCGROUP); err != nil && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("unshare cgroupns: %w", err)
	}
	if err := rootfs.MakePrivate(); err != nil {
		return err
	}
	cfg := &rootfs.Config{
		Layers:  s.Layers,
		Upper:   s.Upper,
		Work:    s.Work,
		Target:  s.Rootfs,
		Empty:   s.Empty,
		Binds:   s.Binds,
		HostNet: s.Network == NetworkHost,
	}
	if err := rootfs.MountOverlay(cfg); err != nil {
		return err
	}
	if err := rootfs.Populate(s.Rootfs, cfg); err != nil {
		return err
	}
	if s.ReadOnly {
		if err := syscall.Mount("", s.Rootfs, "", syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("remount rootfs ro: %w", err)
		}
	}
	if err := syscall.Sethostname([]byte(s.Hostname)); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}
	if s.Network == NetworkNone {
		if err := loopbackUp(); err != nil {
			return err
		}
	}
	var err error
	if s.Isolation == IsolationChroot {
		err = rootfs.Chroot(s.Rootfs)
	} else {
		err = rootfs.PivotRoot(s.Rootfs)
	}
	if err != nil {
		return err
	}
	if err := rootfs.Mask(); err != nil {
		return err
	}
	wd := s.WorkDir
	if wd == "" {
		wd = "/"
	}
	if !s.ReadOnly {
		_ = os.MkdirAll(wd, 0o755)
	}
	if err := os.Chdir(wd); err != nil {
		return fmt.Errorf("workdir %s: %w", wd, err)
	}
	return nil
}

func hasEnv(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

func lookPath(file string) (string, error) {
	if strings.Contains(file, "/") {
		if _, err := os.Stat(file); err != nil {
			return "", fmt.Errorf("%s: %w", file, err)
		}
		return file, nil
	}
	p, err := exec.LookPath(file)
	if err != nil {
		return "", fmt.Errorf("%s: executable not found in $PATH", file)
	}
	if !filepath.IsAbs(p) {
		p, _ = filepath.Abs(p)
	}
	return p, nil
}

// droppedCaps are removed from the bounding set of unprivileged
// containers: kernel modules, raw I/O, rebooting, clock changes, MAC
// policy, audit configuration, BPF and perf. Root inside the container
// keeps the rest (chown, net_bind, setuid...) as docker does.
var droppedCaps = []uintptr{
	9,  // CAP_LINUX_IMMUTABLE
	16, // CAP_SYS_MODULE
	17, // CAP_SYS_RAWIO
	20, // CAP_SYS_PACCT
	22, // CAP_SYS_BOOT
	23, // CAP_SYS_NICE
	24, // CAP_SYS_RESOURCE
	25, // CAP_SYS_TIME
	26, // CAP_SYS_TTY_CONFIG
	28, // CAP_LEASE
	30, // CAP_AUDIT_CONTROL
	32, // CAP_MAC_OVERRIDE
	33, // CAP_MAC_ADMIN
	34, // CAP_SYSLOG
	35, // CAP_WAKE_ALARM
	36, // CAP_BLOCK_SUSPEND
	37, // CAP_AUDIT_READ
	38, // CAP_PERFMON
	39, // CAP_BPF
	40, // CAP_CHECKPOINT_RESTORE
}

const prCapbsetDrop = 24

func dropCapabilities() error {
	for _, c := range droppedCaps {
		_, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prCapbsetDrop, c, 0)
		// EINVAL: capability unknown to this kernel.
		if e != 0 && e != syscall.EINVAL {
			return fmt.Errorf("drop capability %d: %w", c, e)
		}
	}
	return nil
}

// loopbackUp brings "lo" up in a fresh network namespace.
func loopbackUp() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer syscall.Close(fd)
	var ifr struct {
		name  [syscall.IFNAMSIZ]byte
		flags uint16
		_     [22]byte
	}
	copy(ifr.name[:], "lo")
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		return fmt.Errorf("SIOCGIFFLAGS lo: %w", e)
	}
	ifr.flags |= syscall.IFF_UP | syscall.IFF_RUNNING
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		return fmt.Errorf("SIOCSIFFLAGS lo: %w", e)
	}
	return nil
}

// reaper is a minimal init: it starts the workload as a child, forwards
// every catchable signal to it, reaps orphaned zombies, and exits with the
// workload's status.
func reaper(path string, args, env []string, errPipe *os.File) int {
	sigc := make(chan os.Signal, 32)
	signal.Notify(sigc)
	proc, err := os.StartProcess(path, args, &os.ProcAttr{
		Env:   env,
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		fmt.Fprintf(errPipe, "start %s: %v", path, err)
		return ExitNotRunnable
	}
	errPipe.Close()
	go func() {
		for s := range sigc {
			if s == syscall.SIGCHLD || s == syscall.SIGURG { // SIGURG: Go preemption
				continue
			}
			_ = proc.Signal(s)
		}
	}()
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return ExitInitFailed
		}
		if pid != proc.Pid {
			continue // an orphan we adopted
		}
		if ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ws.ExitStatus()
	}
}
