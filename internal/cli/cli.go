// Package cli implements the crt command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/baruahs262/container-runtime/internal/container"
	"github.com/baruahs262/container-runtime/internal/image"
)

// Version is overridden at link time.
var Version = "dev"

const usage = `crt - a small container runtime

Usage: crt [--root DIR] COMMAND [ARGS]

Containers:
  run        Run a command in a new container
  ps         List containers
  stop       Stop running containers (SIGTERM, then SIGKILL)
  kill       Send a signal to containers
  rm         Remove containers
  prune      Remove all stopped containers
  logs       Show a detached container's output
  stats      Live CPU / memory / pids usage
  inspect    Show a container's state as JSON
  commit     Create an image from a container's changes

Images:
  build      Build an image from a Buildfile
  pull       Pull an image from a registry
  images     List images
  image      Image subcommands: ls, rm, tag, history, inspect, save,
             load, import, export, squash, prune

Other:
  gc         Delete layers no image or container uses
  bench      Measure container throughput and limit enforcement
  version    Print version

Environment: CRT_ROOT sets the state directory (default /var/lib/crt).
`

// App carries global settings.
type App struct {
	Root         string
	CgroupParent string
	Stdout       io.Writer
	Stderr       io.Writer
}

func (a *App) runtime() *container.Runtime {
	rt := container.New(a.Root)
	if a.CgroupParent != "" {
		rt.CgroupParent = a.CgroupParent
	}
	return rt
}

func (a *App) store() (*image.Store, error) { return image.Open(a.Root) }

// ExitError carries a process exit code out of a command.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Main runs the CLI and returns the process exit code.
func Main(args []string) int {
	a := &App{Stdout: os.Stdout, Stderr: os.Stderr}
	fs := flag.NewFlagSet("crt", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	def := os.Getenv("CRT_ROOT")
	if def == "" {
		def = "/var/lib/crt"
	}
	fs.StringVar(&a.Root, "root", def, "state directory")
	fs.StringVar(&a.CgroupParent, "cgroup-parent", "", "cgroup directory holding containers (default crt)")
	fs.Usage = func() { fmt.Fprint(a.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(a.Stderr, usage)
		return 2
	}
	err := a.dispatch(rest[0], rest[1:])
	var ee ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.Code
	case errors.Is(err, flag.ErrHelp):
		return 0
	default:
		fmt.Fprintln(a.Stderr, "crt:", err)
		return 1
	}
}

func (a *App) dispatch(cmd string, args []string) error {
	needsRoot := map[string]bool{"run": true, "build": true, "stop": true, "kill": true, "bench": true, "stats": true, "commit": true}
	if needsRoot[cmd] && os.Geteuid() != 0 {
		return fmt.Errorf("%s requires root (namespaces, mounts and cgroups)", cmd)
	}
	switch cmd {
	case "run":
		return a.cmdRun(args)
	case "ps":
		return a.cmdPs(args)
	case "stop":
		return a.cmdStop(args)
	case "kill":
		return a.cmdKill(args)
	case "rm":
		return a.cmdRm(args)
	case "prune":
		return a.cmdPrune(args)
	case "logs":
		return a.cmdLogs(args)
	case "stats":
		return a.cmdStats(args)
	case "inspect":
		return a.cmdInspect(args)
	case "commit":
		return a.cmdCommit(args)
	case "build":
		return a.cmdBuild(args)
	case "pull":
		return a.cmdPull(args)
	case "images":
		return a.cmdImages(args)
	case "rmi":
		return a.cmdImageRm(args)
	case "image":
		return a.cmdImage(args)
	case "gc":
		return a.cmdGC(args)
	case "bench":
		return a.cmdBench(args)
	case "version":
		fmt.Fprintln(a.Stdout, "crt", Version)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(a.Stdout, usage)
		return nil
	}
	return fmt.Errorf("unknown command %q (see crt help)", cmd)
}

func (a *App) cmdImage(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: crt image ls|rm|tag|history|inspect|save|load|import|export|squash|prune|build|pull")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return a.cmdImages(rest)
	case "rm":
		return a.cmdImageRm(rest)
	case "tag":
		return a.cmdImageTag(rest)
	case "history":
		return a.cmdImageHistory(rest)
	case "inspect":
		return a.cmdImageInspect(rest)
	case "save":
		return a.cmdImageSave(rest)
	case "load":
		return a.cmdImageLoad(rest)
	case "import":
		return a.cmdImageImport(rest)
	case "export":
		return a.cmdImageExport(rest)
	case "squash":
		return a.cmdImageSquash(rest)
	case "prune":
		return a.cmdGC(rest)
	case "build":
		return a.cmdBuild(rest)
	case "pull":
		return a.cmdPull(rest)
	}
	return fmt.Errorf("unknown image command %q", sub)
}

// newFlags builds a subcommand flag set that prints its own usage.
func (a *App) newFlags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(a.Stderr, "Usage: crt %s\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

func (a *App) table() *tabwriter.Writer {
	return tabwriter.NewWriter(a.Stdout, 0, 4, 3, ' ', 0)
}

// multiFlag collects repeated string flags.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// ParseBytes parses sizes like 512, 64k, 128m, 1.5g (binary units).
func ParseBytes(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.TrimSuffix(strings.TrimSuffix(t, "ib"), "b")
	mult := 1.0
	if n := len(t); n > 0 {
		switch t[n-1] {
		case 'k':
			mult, t = 1<<10, t[:n-1]
		case 'm':
			mult, t = 1<<20, t[:n-1]
		case 'g':
			mult, t = 1<<30, t[:n-1]
		case 't':
			mult, t = 1<<40, t[:n-1]
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * mult), nil
}

// HumanBytes formats a byte count.
func HumanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
