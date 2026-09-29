package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/baruahs262/container-runtime/internal/cgroup"
	"github.com/baruahs262/container-runtime/internal/container"
	"github.com/baruahs262/container-runtime/internal/rootfs"
)

// BuildOptions configures one build.
type BuildOptions struct {
	Buildfile  io.Reader
	ContextDir string
	Tag        string
	NoCache    bool
	Limits     cgroup.Limits // applied to every RUN step
	Network    string        // container network mode for RUN steps
	Binds      []rootfs.Bind // host paths mounted into every RUN step, never committed
	Out        io.Writer
	// Pull fetches a missing FROM image; nil means FROM must be local.
	Pull func(ctx context.Context, ref string) (*Image, error)
}

// Builder turns a Buildfile into an image.
type Builder struct {
	Store   *Store
	Runtime *container.Runtime
}

// buildState is the image under construction.
type buildState struct {
	layers  []string
	config  Config
	history []History
	key     string // cache chain: hash of the base and every step so far
}

func chain(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		io.WriteString(h, p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Build executes every instruction and saves the tagged image.
func (b *Builder) Build(ctx context.Context, o BuildOptions) (*Image, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	ref, err := ParseRef(o.Tag)
	if err != nil {
		return nil, fmt.Errorf("tag: %w", err)
	}
	instrs, err := ParseBuildfile(o.Buildfile)
	if err != nil {
		return nil, err
	}
	if o.ContextDir == "" {
		o.ContextDir = "."
	}
	if o.ContextDir, err = filepath.Abs(o.ContextDir); err != nil {
		return nil, err
	}
	st := &buildState{}
	for i, in := range instrs {
		fmt.Fprintf(o.Out, "Step %d/%d : %s\n", i+1, len(instrs), in)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := b.step(ctx, o, st, in); err != nil {
			return nil, fmt.Errorf("step %d (%s): %w", i+1, in.Cmd, err)
		}
	}
	img := &Image{
		Name: ref.Name, Tag: ref.Tag, Layers: st.layers, Config: st.config,
		History: st.history, Created: time.Now().UTC(), Source: "build",
	}
	if err := b.Store.Save(img); err != nil {
		return nil, err
	}
	fmt.Fprintf(o.Out, "Successfully built %s\nSuccessfully tagged %s\n", short(img.ID), img.Ref())
	return img, nil
}

func (b *Builder) step(ctx context.Context, o BuildOptions, st *buildState, in Instruction) error {
	now := time.Now().UTC()
	meta := func() {
		st.key = chain(st.key, in.String())
		st.history = append(st.history, History{Created: now, CreatedBy: in.String(), EmptyLayer: true})
	}
	expand := func(s string) string { return os.Expand(s, st.lookupEnv) }

	switch in.Cmd {
	case "FROM":
		name := in.Args[0]
		if name == "scratch" {
			st.key = chain("scratch")
			return nil
		}
		img, err := b.Store.Get(name)
		if err != nil && o.Pull != nil {
			fmt.Fprintf(o.Out, " ---> pulling %s\n", name)
			img, err = o.Pull(ctx, name)
		}
		if err != nil {
			return err
		}
		st.layers = append([]string{}, img.Layers...)
		st.config = img.Config
		st.config.Env = append([]string{}, img.Config.Env...)
		st.config.Labels = copyMap(img.Config.Labels)
		st.history = append([]History{}, img.History...)
		st.key = chain(img.ID)
		fmt.Fprintf(o.Out, " ---> %s\n", short(img.ID))
		return nil

	case "ENV":
		kvs, err := parseKV(in)
		if err != nil {
			return err
		}
		for _, kv := range kvs {
			st.setEnv(kv[0], expand(kv[1]))
		}
		meta()
	case "LABEL":
		kvs, err := parseKV(in)
		if err != nil {
			return err
		}
		if st.config.Labels == nil {
			st.config.Labels = map[string]string{}
		}
		for _, kv := range kvs {
			st.config.Labels[kv[0]] = expand(kv[1])
		}
		meta()
	case "WORKDIR":
		wd := expand(in.Raw)
		if !path.IsAbs(wd) {
			wd = path.Join(st.workdir(), wd)
		}
		st.config.WorkingDir = path.Clean(wd)
		meta()
	case "CMD":
		st.config.Cmd = commandArgs(in)
		meta()
	case "ENTRYPOINT":
		st.config.Entrypoint = commandArgs(in)
		meta()
	case "RUN":
		return b.run(ctx, o, st, in)
	case "COPY":
		return b.copy(o, st, in, expand)
	default:
		return fmt.Errorf("unsupported instruction %s", in.Cmd)
	}
	return nil
}

func (st *buildState) workdir() string {
	if st.config.WorkingDir == "" {
		return "/"
	}
	return st.config.WorkingDir
}

func (st *buildState) lookupEnv(k string) string {
	for i := len(st.config.Env) - 1; i >= 0; i-- {
		if kk, v, _ := strings.Cut(st.config.Env[i], "="); kk == k {
			return v
		}
	}
	return ""
}

func (st *buildState) setEnv(k, v string) {
	for i, kv := range st.config.Env {
		if strings.HasPrefix(kv, k+"=") {
			st.config.Env[i] = k + "=" + v
			return
		}
	}
	st.config.Env = append(st.config.Env, k+"="+v)
}

func (st *buildState) addLayer(id, createdBy string) {
	h := History{Created: time.Now().UTC(), CreatedBy: createdBy}
	if id == EmptyLayer {
		h.EmptyLayer = true
	} else {
		st.layers = append(st.layers, id)
		h.Layer = id
	}
	st.history = append(st.history, h)
}

func (b *Builder) run(ctx context.Context, o BuildOptions, st *buildState, in Instruction) error {
	key := chain(st.key, in.String())
	defer func() { st.key = key }()
	if !o.NoCache {
		if id, ok := b.Store.CacheGet(key); ok {
			fmt.Fprintf(o.Out, " ---> Using cache\n ---> %s\n", short(id))
			st.addLayer(id, in.String())
			return nil
		}
	}
	dirs := make([]string, len(st.layers))
	for i, l := range st.layers {
		dirs[i] = b.Store.LayerDir(l)
	}
	spec := &container.Spec{
		Image:    "<build>",
		Layers:   dirs,
		LayerIDs: st.layers,
		Args:     commandArgs(in),
		Env:      st.config.Env,
		WorkDir:  st.workdir(),
		Hostname: "build",
		Limits:   o.Limits,
		Network:  o.Network,
		Binds:    o.Binds,
	}
	res, err := b.Runtime.Run(ctx, spec, container.RunOpts{Stdout: o.Out, Stderr: o.Out})
	if spec.ID != "" {
		defer b.Runtime.Remove(spec.ID, true)
	}
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		msg := fmt.Sprintf("command returned non-zero exit code %d", res.ExitCode)
		if res.OOMKilled {
			msg += " (killed: out of memory)"
		}
		return fmt.Errorf("%s", msg)
	}
	var targets []string
	for _, bd := range o.Binds {
		targets = append(targets, bd.Target)
	}
	if err := PruneUpper(spec.Upper, dirs, targets...); err != nil {
		return err
	}
	id, err := b.Store.CommitDir(spec.Upper, "build: "+in.String())
	if err != nil {
		return err
	}
	if err := b.Store.CachePut(key, id); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, " ---> %s\n", short(id))
	st.addLayer(id, in.String())
	return nil
}

func (b *Builder) copy(o BuildOptions, st *buildState, in Instruction, expand func(string) string) error {
	if len(in.Args) < 2 {
		return fmt.Errorf("COPY needs at least one source and a destination")
	}
	args := make([]string, len(in.Args))
	for i, a := range in.Args {
		args[i] = expand(a)
	}
	srcs, rawDest := args[:len(args)-1], args[len(args)-1]
	dest := rawDest
	if !path.IsAbs(dest) {
		dest = path.Join(st.workdir(), dest)
	}
	dest = path.Clean(dest)

	var matches []string
	for _, s := range srcs {
		m, err := filepath.Glob(filepath.Join(o.ContextDir, s))
		if err != nil {
			return err
		}
		if len(m) == 0 {
			return fmt.Errorf("COPY source %q not found in build context", s)
		}
		sort.Strings(m)
		for _, p := range m {
			rel, err := filepath.Rel(o.ContextDir, p)
			if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				return fmt.Errorf("COPY source %q is outside the build context", s)
			}
		}
		matches = append(matches, m...)
	}
	destIsDir := strings.HasSuffix(rawDest, "/") || len(matches) > 1
	if !destIsDir {
		if fi, err := os.Stat(matches[0]); err == nil && fi.IsDir() {
			destIsDir = true
		}
	}

	stage, err := b.Store.TempDir("copy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	layer := filepath.Join(stage, "fs")
	if err := os.Mkdir(layer, 0o755); err != nil {
		return err
	}
	lowers := make([]string, len(st.layers))
	for i, l := range st.layers {
		lowers[i] = b.Store.LayerDir(l)
	}
	for _, src := range matches {
		fi, err := os.Stat(src)
		if err != nil {
			return err
		}
		target := dest
		if destIsDir && !fi.IsDir() {
			target = path.Join(dest, filepath.Base(src))
		}
		if err := mkdirLike(layer, path.Dir(target), lowers); err != nil {
			return err
		}
		if fi.IsDir() {
			if err := mkdirLike(layer, target, lowers); err != nil {
				return err
			}
			err = copyTree(src, filepath.Join(layer, target))
		} else {
			err = copyFile(src, filepath.Join(layer, target), fi.Mode())
		}
		if err != nil {
			return err
		}
	}
	id, err := b.Store.CommitDir(layer, "build: "+in.String())
	if err != nil {
		return err
	}
	st.key = chain(st.key, in.String(), id)
	fmt.Fprintf(o.Out, " ---> %s\n", short(id))
	st.addLayer(id, in.String())
	return nil
}

// mkdirLike creates dir (absolute, container view) inside layer, giving
// each new component the mode and owner it has in the lower layers so the
// upper copy doesn't change permissions of e.g. /tmp.
func mkdirLike(layer, dir string, lowers []string) error {
	cur := ""
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part == "" {
			continue
		}
		cur = path.Join(cur, part)
		p := filepath.Join(layer, cur)
		if fi, err := os.Lstat(p); err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", cur)
			}
			continue
		}
		mode, uid, gid := os.FileMode(0o755), 0, 0
		for i := len(lowers) - 1; i >= 0; i-- {
			if fi, err := os.Lstat(filepath.Join(lowers[i], cur)); err == nil {
				if fi.IsDir() {
					s := fi.Sys().(*syscall.Stat_t)
					mode = fi.Mode().Perm() | fi.Mode()&(fs.ModeSticky|fs.ModeSetgid)
					uid, gid = int(s.Uid), int(s.Gid)
				}
				break
			}
		}
		if err := os.Mkdir(p, 0o700); err != nil {
			return err
		}
		os.Lchown(p, uid, gid)
		if err := os.Chmod(p, mode); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	os.RemoveAll(dst)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	os.Lchown(dst, 0, 0)
	return os.Chmod(dst, mode.Perm()|mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
}

// copyTree copies the contents of src into dst (which exists), owned by
// root, preserving modes and symlinks.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		t := filepath.Join(dst, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			if err := os.MkdirAll(t, 0o755); err != nil {
				return err
			}
			os.Lchown(t, 0, 0)
			return os.Chmod(t, fi.Mode().Perm())
		case fi.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			os.RemoveAll(t)
			if err := os.Symlink(link, t); err != nil {
				return err
			}
			return os.Lchown(t, 0, 0)
		case fi.Mode().IsRegular():
			return copyFile(p, t, fi.Mode())
		}
		return nil // skip devices, sockets, fifos from the build context
	})
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
