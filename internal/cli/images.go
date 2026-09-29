package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/baruahs262/container-runtime/internal/cgroup"
	"github.com/baruahs262/container-runtime/internal/image"
)

func (a *App) cmdBuild(args []string) error {
	fs := a.newFlags("build", "build -t NAME[:TAG] [-f Buildfile] [OPTIONS] CONTEXT")
	tag := fs.String("t", "", "name and optional tag for the image (required)")
	file := fs.String("f", "", "Buildfile path (default CONTEXT/Buildfile, then CONTEXT/Dockerfile)")
	noCache := fs.Bool("no-cache", false, "do not use the build cache")
	cpu := fs.Float64("cpu", 0, "CPU limit for RUN steps")
	mem := fs.String("memory", "", "memory limit for RUN steps")
	network := fs.String("network", "host", "network for RUN steps: host | none")
	pull := fs.Bool("pull", true, "pull the FROM image if it is not present")
	var volumes multiFlag
	fs.Var(&volumes, "v", "bind mount HOST:CONTAINER[:ro] into RUN steps; not committed (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	binds, err := parseVolumes(volumes)
	if err != nil {
		return err
	}
	if *tag == "" || fs.NArg() > 1 {
		fs.Usage()
		return ExitError{2}
	}
	ctxDir := "."
	if fs.NArg() == 1 {
		ctxDir = fs.Arg(0)
	}
	path := *file
	if path == "" {
		path = filepath.Join(ctxDir, "Buildfile")
		if _, err := os.Stat(path); err != nil {
			path = filepath.Join(ctxDir, "Dockerfile")
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := a.store()
	if err != nil {
		return err
	}
	limits := cgroup.Limits{CPU: *cpu}
	if *mem != "" {
		if limits.Memory, err = ParseBytes(*mem); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := image.BuildOptions{
		Buildfile: f, ContextDir: ctxDir, Tag: *tag, NoCache: *noCache,
		Limits: limits, Network: *network, Out: a.Stdout, Binds: binds,
	}
	if *pull {
		opts.Pull = func(ctx context.Context, ref string) (*image.Image, error) {
			return st.Pull(ctx, ref, image.PullOptions{Out: a.Stdout})
		}
	}
	b := &image.Builder{Store: st, Runtime: a.runtime()}
	_, err = b.Build(ctx, opts)
	return err
}

func (a *App) cmdPull(args []string) error {
	fs := a.newFlags("pull", "pull [--arch ARCH] IMAGE[:TAG]")
	arch := fs.String("arch", "", "architecture (default: this machine's)")
	jobs := fs.Int("j", 4, "parallel layer downloads")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return ExitError{2}
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, err = st.Pull(ctx, fs.Arg(0), image.PullOptions{Arch: *arch, Concurrency: *jobs, Out: a.Stdout})
	return err
}

func (a *App) cmdImages(args []string) error {
	fs := a.newFlags("images", "images [-q]")
	quiet := fs.Bool("q", false, "only print ids")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	imgs, err := st.List()
	if err != nil {
		return err
	}
	tw := a.table()
	if !*quiet {
		fmt.Fprintln(tw, "REPOSITORY\tTAG\tIMAGE ID\tLAYERS\tSIZE\tCREATED")
	}
	for _, img := range imgs {
		if *quiet {
			fmt.Fprintln(tw, image.Short(img.ID))
			continue
		}
		var size int64
		for _, l := range img.Layers {
			if m, err := st.GetLayer(l); err == nil {
				size += m.Size
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", img.Name, img.Tag, image.Short(img.ID), len(img.Layers), HumanBytes(uint64(size)), ago(img.Created))
	}
	return tw.Flush()
}

func (a *App) cmdImageRm(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: crt image rm IMAGE...")
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	return a.each(args, func(ref string) error {
		_, err := st.Remove(ref)
		return err
	})
}

func (a *App) cmdImageTag(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: crt image tag SOURCE TARGET")
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	_, err = st.Tag(args[0], args[1])
	return err
}

func (a *App) cmdImageHistory(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: crt image history IMAGE")
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	img, err := st.Get(args[0])
	if err != nil {
		return err
	}
	tw := a.table()
	fmt.Fprintln(tw, "LAYER\tCREATED\tCREATED BY\tSIZE")
	for i := len(img.History) - 1; i >= 0; i-- {
		h := img.History[i]
		layer, size := "<missing>", "0B"
		if h.EmptyLayer {
			layer = "-"
		} else if h.Layer != "" {
			layer = image.Short(h.Layer)
			if m, err := st.GetLayer(h.Layer); err == nil {
				size = HumanBytes(uint64(m.Size))
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", layer, ago(h.Created), truncate(h.CreatedBy, 60), size)
	}
	return tw.Flush()
}

func (a *App) cmdImageInspect(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: crt image inspect IMAGE")
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	img, err := st.Get(args[0])
	if err != nil {
		return err
	}
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(img)
}

func (a *App) input(path string) (io.ReadCloser, error) {
	if path == "" || path == "-" {
		return io.NopCloser(os.Stdin), nil
	}
	return os.Open(path)
}

func (a *App) cmdImageSave(args []string) error {
	fs := a.newFlags("image save", "image save [-o FILE] IMAGE")
	out := fs.String("o", "-", "output file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
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
	return a.writeTo(*out, func(w io.Writer) error { return st.SaveBundle(img, w) })
}

// writeTo streams fn's output to path ("-" or "" meaning stdout). A
// failed write removes the partial file.
func (a *App) writeTo(path string, fn func(io.Writer) error) error {
	if path == "" || path == "-" {
		bw := bufio.NewWriterSize(a.Stdout, 1<<20)
		if err := fn(bw); err != nil {
			return err
		}
		return bw.Flush()
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	err = fn(bw)
	if err == nil {
		err = bw.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

func (a *App) cmdImageLoad(args []string) error {
	fs := a.newFlags("image load", "image load [-i FILE]")
	in := fs.String("i", "-", "bundle file written by image save")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, err := a.input(*in)
	if err != nil {
		return err
	}
	defer r.Close()
	st, err := a.store()
	if err != nil {
		return err
	}
	img, err := st.LoadBundle(bufio.NewReaderSize(r, 1<<20))
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Loaded %s (%s)\n", img.Ref(), image.Short(img.ID))
	return nil
}

func (a *App) cmdImageImport(args []string) error {
	fs := a.newFlags("image import", "image import [--cmd CMD] FILE|- NAME[:TAG]")
	cmd := fs.String("cmd", "", "default command, e.g. /bin/sh")
	var env multiFlag
	fs.Var(&env, "e", "default environment KEY=VALUE (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return ExitError{2}
	}
	r, err := a.input(fs.Arg(0))
	if err != nil {
		return err
	}
	defer r.Close()
	st, err := a.store()
	if err != nil {
		return err
	}
	cfg := image.Config{Env: env}
	if *cmd != "" {
		cfg.Cmd = strings.Fields(*cmd)
	}
	if len(cfg.Env) == 0 {
		cfg.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	}
	img, err := st.ImportRootfs(bufio.NewReaderSize(r, 1<<20), fs.Arg(1), cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, img.ID)
	return nil
}

func (a *App) cmdImageExport(args []string) error {
	fs := a.newFlags("image export", "image export [-o FILE] IMAGE")
	out := fs.String("o", "-", "output tarball of the merged root filesystem")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
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
	return a.writeTo(*out, func(w io.Writer) error { return st.ExportRootfs(img, w) })
}

func (a *App) cmdImageSquash(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: crt image squash IMAGE NEW-NAME[:TAG]")
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	img, err := st.Get(args[0])
	if err != nil {
		return err
	}
	out, err := st.Squash(img, args[1])
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "%s: %d layers -> 1 (%s)\n", out.Ref(), len(img.Layers), image.Short(out.ID))
	return nil
}

func (a *App) cmdGC(args []string) error {
	st, err := a.store()
	if err != nil {
		return err
	}
	list, err := a.runtime().List()
	if err != nil {
		return err
	}
	var keep []string
	for _, c := range list {
		keep = append(keep, c.Spec.LayerIDs...)
	}
	res, err := st.GC(keep)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Deleted %d layers, reclaimed %s\n", len(res.Layers), HumanBytes(uint64(res.Bytes)))
	return nil
}
