// Package integration drives the compiled crt binary end to end. It needs
// root; without it every test skips.
package integration

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var (
	bin    string // directory with crt and crt-stress
	stateR string // CRT_ROOT for the whole run
)

func TestMain(m *testing.M) {
	if os.Geteuid() != 0 {
		os.Exit(m.Run()) // tests skip themselves
	}
	tmp, err := os.MkdirTemp("", "crt-it-")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(tmp, "bin")
	stateR = filepath.Join(tmp, "state")
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(file), "..", "..")
	for _, target := range []string{"crt", "crt-stress"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(bin, target), "./cmd/"+target)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			panic(string(out))
		}
	}
	// The stress image: FROM scratch + the static workload.
	if out, err := crt("build", "-t", "stress:it", "-f", filepath.Join(repo, "examples", "Buildfile.stress"), bin); err != nil {
		panic(out)
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

func crt(args ...string) (string, error) {
	cmd := exec.Command(filepath.Join(bin, "crt"), args...)
	cmd.Env = append(os.Environ(), "CRT_ROOT="+stateR)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func exitCode(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func root(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
}

func TestRunDefaultCommandAndExitCodes(t *testing.T) {
	root(t)
	if out, err := crt("run", "--rm", "stress:it"); err != nil {
		t.Fatalf("default CMD: %v %s", err, out)
	}
	out, err := crt("run", "--rm", "stress:it", "crt-stress", "-echo", "hi", "-exit", "9")
	if exitCode(err) != 9 || !strings.Contains(out, "hi") {
		t.Errorf("exit %d out %q", exitCode(err), out)
	}
	out, err = crt("run", "--rm", "stress:it", "no-such-binary")
	if exitCode(err) != 127 || !strings.Contains(out, "not found") {
		t.Errorf("missing binary: exit %d out %q", exitCode(err), out)
	}
}

func TestOOMReportedByCLI(t *testing.T) {
	root(t)
	out, err := crt("run", "--rm", "--memory", "24m", "--stats", "stress:it", "crt-stress", "-mem", "200")
	if exitCode(err) != 137 || !strings.Contains(out, "out of memory") || !strings.Contains(out, "oom kills 1") {
		t.Errorf("exit %d\n%s", exitCode(err), out)
	}
}

func TestCPUQuotaSummary(t *testing.T) {
	root(t)
	out, err := crt("run", "--rm", "--cpu", "0.25", "--stats", "stress:it", "crt-stress", "-cpu", "1500ms")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "limit 25%") || strings.Contains(out, "throttled 0 periods") {
		t.Errorf("unexpected summary:\n%s", out)
	}
}

func TestDetachedLifecycle(t *testing.T) {
	root(t)
	id, err := crt("run", "-d", "--init", "stress:it", "crt-stress", "-echo", "ready", "-sleep", "60s")
	if err != nil {
		t.Fatalf("%v %s", err, id)
	}
	id = strings.TrimSpace(id)
	deadline := time.Now().Add(5 * time.Second)
	for {
		logs, _ := crt("logs", id)
		if strings.Contains(logs, "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no output in logs: %q", logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ps, _ := crt("ps")
	if !strings.Contains(ps, id) || !strings.Contains(ps, "Up") {
		t.Errorf("ps:\n%s", ps)
	}
	if out, err := crt("stats", "--no-stream", id); err != nil || !strings.Contains(out, id) {
		t.Errorf("stats: %v\n%s", err, out)
	}
	if _, err := crt("rm", id); err == nil {
		t.Error("rm of running container succeeded")
	}
	if out, err := crt("stop", "-t", "3", id[:6]); err != nil {
		t.Fatalf("stop: %v %s", err, out)
	}
	ps, _ = crt("ps", "-a")
	if !strings.Contains(ps, "Exited (143)") {
		t.Errorf("after stop:\n%s", ps)
	}
	if out, err := crt("rm", id); err != nil {
		t.Errorf("rm: %v %s", err, out)
	}
}

func TestCommitSaveLoadGC(t *testing.T) {
	root(t)
	out, err := crt("run", "stress:it", "crt-stress", "-write", "/state")
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	ps, _ := crt("ps", "-a", "-q")
	id := strings.Fields(ps)[0]
	if out, err := crt("commit", id, "stress:committed"); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	if out, err := crt("run", "--rm", "stress:committed", "crt-stress", "-read", "/state"); err != nil || out != "marker\n" {
		t.Fatalf("committed file: %v %q", err, out)
	}
	bundle := filepath.Join(t.TempDir(), "img.bundle")
	if out, err := crt("image", "save", "-o", bundle, "stress:committed"); err != nil {
		t.Fatalf("save: %v %s", err, out)
	}
	crt("rm", id)
	crt("image", "rm", "stress:committed")
	if out, _ := crt("gc"); !strings.Contains(out, "Deleted 1 layers") {
		t.Errorf("gc: %s", out)
	}
	if out, err := crt("image", "load", "-i", bundle); err != nil {
		t.Fatalf("load: %v %s", err, out)
	}
	if out, err := crt("run", "--rm", "stress:committed", "crt-stress", "-read", "/state"); err != nil || out != "marker\n" {
		t.Errorf("after load: %v %q", err, out)
	}
	hist, _ := crt("image", "history", "stress:committed")
	if !strings.Contains(hist, "commit:") || !strings.Contains(hist, "COPY crt-stress /bin/") {
		t.Errorf("history:\n%s", hist)
	}
}

func TestBench(t *testing.T) {
	root(t)
	out, err := crt("bench", "-n", "20", "-parallel", "4", "-enforcement=false", "-stress-bin", filepath.Join(bin, "crt-stress"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"host fork/exec", "no limits", "+ limits", "monitor goroutine"} {
		if !strings.Contains(out, want) {
			t.Errorf("bench output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "first error") {
		t.Errorf("bench had failures:\n%s", out)
	}
}
