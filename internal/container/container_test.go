package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baruahs262/container-runtime/internal/cgroup"
)

var (
	layerOnce sync.Once
	layerDir  string
	layerErr  error
)

// stressLayer builds cmd/crt-stress statically into a scratch-style layer.
func stressLayer(t testing.TB) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	layerOnce.Do(func() {
		dir, err := os.MkdirTemp("", "crt-layer-")
		if err != nil {
			layerErr = err
			return
		}
		_, file, _, _ := runtime.Caller(0)
		repo := filepath.Join(filepath.Dir(file), "..", "..")
		os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
		os.MkdirAll(filepath.Join(dir, "tmp"), 0o1777)
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "bin", "crt-stress"), "./cmd/crt-stress")
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			layerErr = errors.New(string(out))
			return
		}
		layerDir = dir
	})
	if layerErr != nil {
		t.Fatal(layerErr)
	}
	return layerDir
}

func testRuntime(t testing.TB) *Runtime {
	r := New(t.TempDir())
	r.CgroupParent = "crt-test"
	return r
}

func run(t testing.TB, r *Runtime, spec *Spec, o RunOpts) (*Result, string) {
	t.Helper()
	var out bytes.Buffer
	o.Stdout, o.Stderr = &out, &out
	res, err := r.Run(context.Background(), spec, o)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	return res, out.String()
}

func TestRunNamespaces(t *testing.T) {
	layer := stressLayer(t)
	r := testRuntime(t)
	res, out := run(t, r, &Spec{
		Layers:   []string{layer},
		Args:     []string{"crt-stress", "-pid", "-hostname", "-cwd", "-env", "FOO", "-exit", "3"},
		Env:      []string{"FOO=bar"},
		Hostname: "box",
		WorkDir:  "/work",
	}, RunOpts{})
	if res.ExitCode != 3 {
		t.Errorf("exit = %d", res.ExitCode)
	}
	want := "1\nbox\n/work\nbar\n"
	if out != want {
		t.Errorf("output %q, want %q", out, want)
	}
}

func TestRunChroot(t *testing.T) {
	layer := stressLayer(t)
	r := testRuntime(t)
	_, out := run(t, r, &Spec{
		Layers:    []string{layer},
		Isolation: IsolationChroot,
		Args:      []string{"/bin/crt-stress", "-ls", "/"},
	}, RunOpts{})
	for _, d := range []string{"bin", "dev", "proc", "sys"} {
		if !strings.Contains(out, d) {
			t.Errorf("root listing %q missing %s", out, d)
		}
	}
}

func TestOverlayCopyOnWrite(t *testing.T) {
	layer := stressLayer(t)
	r := testRuntime(t)
	spec := &Spec{Layers: []string{layer}, Args: []string{"crt-stress", "-write", "/tmp/hello", "-rm", "/bin/crt-stress"}}
	res, out := run(t, r, spec, RunOpts{})
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, out)
	}
	if _, err := os.Stat(filepath.Join(layer, "tmp", "hello")); err == nil {
		t.Error("write leaked into the image layer")
	}
	if _, err := os.Stat(filepath.Join(spec.Upper, "tmp", "hello")); err != nil {
		t.Errorf("write not in upper: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layer, "bin", "crt-stress")); err != nil {
		t.Error("delete reached the image layer")
	}
	fi, err := os.Lstat(filepath.Join(spec.Upper, "bin", "crt-stress"))
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		t.Errorf("expected whiteout in upper, got %v %v", fi, err)
	}
}

func TestMissingBinary(t *testing.T) {
	layer := stressLayer(t)
	r := testRuntime(t)
	res, err := r.Run(context.Background(), &Spec{Layers: []string{layer}, Args: []string{"nope"}}, RunOpts{})
	if !errors.Is(err, ErrInit) || res.ExitCode != ExitNotFound {
		t.Fatalf("got %v exit %d", err, res.ExitCode)
	}
}

func TestMemoryLimitOOM(t *testing.T) {
	layer := stressLayer(t)
	r := testRuntime(t)
	res, out := run(t, r, &Spec{
		Layers: []string{layer},
		Args:   []string{"crt-stress", "-mem", "256"},
		Limits: cgroup.Limits{Memory: 32 << 20},
	}, RunOpts{MonitorInterval: 20 * time.Millisecond})
	if !res.OOMKilled || res.ExitCode != 137 {
		t.Fatalf("expected OOM kill, got exit %d oom=%v: %s", res.ExitCode, res.OOMKilled, out)
	}
	if res.Summary.PeakMem > 33<<20 {
		t.Errorf("peak memory %d above limit", res.Summary.PeakMem)
	}
}

func TestCPULimit(t *testing.T) {
	layer := stressLayer(t)
	r := testRuntime(t)
	res, out := run(t, r, &Spec{
		Layers: []string{layer},
		Args:   []string{"crt-stress", "-cpu", "2s"},
		Limits: cgroup.Limits{CPU: 0.5},
	}, RunOpts{MonitorInterval: 100 * time.Millisecond})
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, out)
	}
	if res.Summary.AvgCPU > 60 || res.Summary.AvgCPU < 30 {
		t.Errorf("avg cpu %.1f%%, want ~50%%", res.Summary.AvgCPU)
	}
	if res.Summary.Throttled == 0 {
		t.Error("never throttled")
	}
}

func TestPidsLimit(t *testing.T) {
	layer := stressLayer(t)
	r := testRuntime(t)
	res, out := run(t, r, &Spec{
		Layers: []string{layer},
		Args:   []string{"crt-stress", "-fork", "50"},
		Limits: cgroup.Limits{Pids: 20},
	}, RunOpts{})
	if res.ExitCode == 0 {
		t.Fatalf("fork bomb not contained: %s", out)
	}
}

func TestNetworkNone(t *testing.T) {
	layer := stressLayer(t)
	r := testRuntime(t)
	_, out := run(t, r, &Spec{
		Layers:  []string{layer},
		Network: NetworkNone,
		Args:    []string{"crt-stress", "-ls", "/sys/class/net"},
	}, RunOpts{})
	if strings.TrimSpace(out) != "lo" {
		t.Errorf("interfaces = %q, want only lo", out)
	}
}

func TestInitReaper(t *testing.T) {
	layer := stressLayer(t)
	r := testRuntime(t)
	res, out := run(t, r, &Spec{
		Layers: []string{layer}, Init: true,
		Args: []string{"crt-stress", "-pid", "-exit", "4"},
	}, RunOpts{})
	// The Go init's own threads take the low pids; the workload is
	// simply not PID 1.
	if res.ExitCode != 4 || strings.TrimSpace(out) == "1" {
		t.Errorf("exit %d pid %q; want 4 and a pid other than 1", res.ExitCode, out)
	}
}

func TestDetachStopRemove(t *testing.T) {
	layer := stressLayer(t)
	r := testRuntime(t)
	st, err := r.Detach(&Spec{Layers: []string{layer}, Init: true, Args: []string{"crt-stress", "-echo", "up", "-sleep", "60s"}}, RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	id := st.Spec.ID
	if st.Status != StatusRunning {
		t.Fatalf("status %s", st.Status)
	}
	list, _ := r.List()
	if len(list) != 1 || list[0].Status != StatusRunning {
		t.Fatalf("list = %+v", list)
	}
	if err := r.Remove(id[:6], false); err == nil {
		t.Fatal("removed a running container without force")
	}
	waitFor(t, func() bool {
		b, _ := os.ReadFile(r.LogPath(id))
		return strings.Contains(string(b), "up")
	})
	t0 := time.Now()
	if err := r.Stop(id[:6], 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(t0) > 4*time.Second {
		t.Errorf("stop took %s; SIGTERM should have been forwarded by init", time.Since(t0))
	}
	st, _ = r.Load(id)
	if st.Status != StatusExited || st.ExitCode != 128+15 {
		t.Errorf("after stop: %s exit %d", st.Status, st.ExitCode)
	}
	if err := r.Remove(id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.Dir(id)); !os.IsNotExist(err) {
		t.Error("container dir survived rm")
	}
}

func waitFor(t testing.TB, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out waiting for condition")
}

// BenchmarkRun measures one full container lifecycle: cgroup create,
// clone into namespaces, overlay mount, pivot_root, exec, wait, teardown.
func BenchmarkRun(b *testing.B) {
	layer := stressLayer(b)
	r := testRuntime(b)
	for i := 0; i < b.N; i++ {
		res, err := r.Run(context.Background(), &Spec{Layers: []string{layer}, Args: []string{"crt-stress"}},
			RunOpts{Stdout: io.Discard, Stderr: io.Discard, Remove: true})
		if err != nil || res.ExitCode != 0 {
			b.Fatal(err, res)
		}
	}
}

// BenchmarkRunParallel runs lifecycles from GOMAXPROCS goroutines.
func BenchmarkRunParallel(b *testing.B) {
	layer := stressLayer(b)
	r := testRuntime(b)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := r.Run(context.Background(), &Spec{Layers: []string{layer}, Args: []string{"crt-stress"}},
				RunOpts{Stdout: io.Discard, Stderr: io.Discard, Remove: true})
			if err != nil || res.ExitCode != 0 {
				b.Error(err, res)
				return
			}
		}
	})
}

// BenchmarkCgroupLifecycle isolates cgroup create + limit + destroy.
func BenchmarkCgroupLifecycle(b *testing.B) {
	if os.Geteuid() != 0 {
		b.Skip("needs root")
	}
	for i := 0; i < b.N; i++ {
		m, err := cgroup.New("", "crt-test", NewID())
		if err != nil {
			b.Fatal(err)
		}
		if err := m.Create(); err != nil {
			b.Fatal(err)
		}
		if err := m.Set(cgroup.Limits{CPU: 0.5, Memory: 64 << 20, Pids: 64}); err != nil {
			b.Fatal(err)
		}
		if err := m.Destroy(); err != nil {
			b.Fatal(err)
		}
	}
}
