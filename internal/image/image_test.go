package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/baruahs262/container-runtime/internal/container"
)

func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		in, name, tag, host, repo string
	}{
		{"alpine", "alpine", "latest", "registry-1.docker.io", "library/alpine"},
		{"alpine:3.20", "alpine", "3.20", "registry-1.docker.io", "library/alpine"},
		{"user/app:v1", "user/app", "v1", "registry-1.docker.io", "user/app"},
		{"docker.io/library/busybox", "docker.io/library/busybox", "latest", "registry-1.docker.io", "library/busybox"},
		{"ghcr.io/org/tool:1.2", "ghcr.io/org/tool", "1.2", "ghcr.io", "org/tool"},
		{"localhost:5000/x", "localhost:5000/x", "latest", "localhost:5000", "x"},
	}
	for _, c := range cases {
		r, err := ParseRef(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		host, repo := r.Registry()
		if r.Name != c.name || r.Tag != c.tag || host != c.host || repo != c.repo {
			t.Errorf("%s: got %+v %s %s", c.in, r, host, repo)
		}
	}
	r, _ := ParseRef("alpine@sha256:abc")
	if r.Digest != "sha256:abc" || r.Tag != "" {
		t.Errorf("digest ref: %+v", r)
	}
	for _, bad := range []string{"", "Upper", "a b", "x@md5:1"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseBuildfile(t *testing.T) {
	src := `# comment
FROM alpine:3.20
ENV A=1 B="two words"
env LEGACY some value
RUN apk add \
    curl \
    # inline comment
    jq
COPY ["a b.txt", "/dst/"]
ADD src /app
WORKDIR app
CMD ["/bin/app", "--flag"]
ENTRYPOINT echo hi
LABEL k=v
`
	ins, err := ParseBuildfile(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	var cmds []string
	for _, in := range ins {
		cmds = append(cmds, in.Cmd)
	}
	want := []string{"FROM", "ENV", "ENV", "RUN", "COPY", "COPY", "WORKDIR", "CMD", "ENTRYPOINT", "LABEL"}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("cmds = %v", cmds)
	}
	if ins[3].Raw != "apk add      curl      jq" && !strings.Contains(ins[3].Raw, "curl") {
		t.Errorf("continuation: %q", ins[3].Raw)
	}
	if strings.Contains(ins[3].Raw, "#") {
		t.Errorf("comment leaked into RUN: %q", ins[3].Raw)
	}
	if !ins[4].JSON || ins[4].Args[0] != "a b.txt" {
		t.Errorf("json COPY: %+v", ins[4])
	}
	kv, _ := parseKV(ins[1])
	if len(kv) != 2 || kv[1] != [2]string{"B", "two words"} {
		t.Errorf("ENV kv = %v", kv)
	}
	kv, _ = parseKV(ins[2])
	if kv[0] != [2]string{"LEGACY", "some value"} {
		t.Errorf("legacy ENV = %v", kv)
	}
	if got := commandArgs(ins[8]); !reflect.DeepEqual(got, []string{"/bin/sh", "-c", "echo hi"}) {
		t.Errorf("shell form = %v", got)
	}
	if got := commandArgs(ins[7]); !reflect.DeepEqual(got, []string{"/bin/app", "--flag"}) {
		t.Errorf("exec form = %v", got)
	}

	for _, bad := range []string{"", "RUN x", "FROM a\nFROM b", "FROM a\nUSER x", "FROM"} {
		if _, err := ParseBuildfile(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	s, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"`)
	if s != "bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" ||
		p["scope"] != "repository:library/alpine:pull" {
		t.Errorf("got %s %v", s, p)
	}
}

func TestSelectPlatform(t *testing.T) {
	var m manifest
	json.Unmarshal([]byte(`{"manifests":[
		{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}},
		{"digest":"sha256:amd","platform":{"os":"linux","architecture":"amd64"}}]}`), &m)
	if d, err := selectPlatform(&m, "linux", "amd64"); err != nil || d != "sha256:amd" {
		t.Errorf("got %s %v", d, err)
	}
	if _, err := selectPlatform(&m, "linux", "s390x"); err == nil {
		t.Error("expected error for missing platform")
	}
}

func TestTreeHashIgnoresTime(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, d := range []string{a, b} {
		os.MkdirAll(filepath.Join(d, "x/y"), 0o755)
		os.WriteFile(filepath.Join(d, "x/y/f"), []byte("hi"), 0o644)
		os.Symlink("y/f", filepath.Join(d, "x/l"))
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(b, "x/y/f"), old, old)
	ha, _ := TreeHash(a)
	hb, _ := TreeHash(b)
	if ha.ID != hb.ID || ha.Files != 4 || ha.Bytes != 2 {
		t.Fatalf("hashes differ or wrong counts: %+v %+v", ha, hb)
	}
	os.Chmod(filepath.Join(b, "x/y/f"), 0o755)
	if hb2, _ := TreeHash(b); hb2.ID == ha.ID {
		t.Error("mode change not detected")
	}
}

type tarFile struct {
	name, body, link string
	typ              byte
	mode             int64
}

func mkTar(t *testing.T, files []tarFile, gz bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.Writer = &buf
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = zw
	}
	tw := tar.NewWriter(w)
	for _, f := range files {
		h := &tar.Header{Name: f.name, Typeflag: f.typ, Mode: f.mode, Linkname: f.link, Size: int64(len(f.body)), ModTime: time.Unix(1e9, 0)}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if f.typ == tar.TypeDir || f.typ == tar.TypeSymlink || f.typ == tar.TypeLink {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			tw.Write([]byte(f.body))
		}
	}
	tw.Close()
	if zw != nil {
		zw.Close()
	}
	return buf.Bytes()
}

func TestExtractWhiteoutsAndRoundTrip(t *testing.T) {
	needRoot(t)
	dir := t.TempDir()
	data := mkTar(t, []tarFile{
		{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		{name: "etc/passwd", typ: tar.TypeReg, body: "root"},
		{name: "etc/link", typ: tar.TypeLink, link: "etc/passwd"},
		{name: "etc/sym", typ: tar.TypeSymlink, link: "passwd"},
		{name: "etc/.wh.shadow", typ: tar.TypeReg},
		{name: "var/cache/.wh..wh..opq", typ: tar.TypeReg},
		{name: "bin/su", typ: tar.TypeReg, body: "x", mode: 0o4755},
	}, true)
	if _, err := ExtractAuto(bytes.NewReader(data), dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(dir, "etc/shadow"))
	if err != nil || !isWhiteout(fi) {
		t.Errorf("whiteout not converted: %v %v", fi, err)
	}
	if !isOpaque(filepath.Join(dir, "var/cache")) {
		t.Error("opaque marker not converted")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "bin/su")); fi.Mode()&os.ModeSetuid == 0 {
		t.Error("setuid bit lost")
	}
	a, _ := os.Stat(filepath.Join(dir, "etc/passwd"))
	b, _ := os.Stat(filepath.Join(dir, "etc/link"))
	if !os.SameFile(a, b) {
		t.Error("hardlink not preserved")
	}

	// Round trip through the OCI layer format must be lossless.
	var buf bytes.Buffer
	if err := WriteLayerTar(dir, &buf); err != nil {
		t.Fatal(err)
	}
	dir2 := t.TempDir()
	if _, err := Extract(&buf, dir2); err != nil {
		t.Fatal(err)
	}
	h1, _ := TreeHash(dir)
	h2, _ := TreeHash(dir2)
	if h1.ID != h2.ID {
		t.Errorf("round trip changed layer: %s vs %s", h1.ID, h2.ID)
	}
}

func TestExtractRejectsEscapes(t *testing.T) {
	for _, files := range [][]tarFile{
		{{name: "../../evil", typ: tar.TypeReg, body: "x"}},
		{{name: "l", typ: tar.TypeSymlink, link: "/tmp"}, {name: "l/evil", typ: tar.TypeReg, body: "x"}},
	} {
		dir := t.TempDir()
		_, err := Extract(bytes.NewReader(mkTar(t, files, false)), dir)
		if files[0].name == "../../evil" {
			// Lexical escapes are clamped inside the root.
			if _, serr := os.Stat(filepath.Join(dir, "evil")); err != nil || serr != nil {
				t.Errorf("clamp failed: %v %v", err, serr)
			}
			continue
		}
		if err == nil {
			t.Error("symlink traversal accepted")
		}
	}
}

func TestFlatten(t *testing.T) {
	needRoot(t)
	l1, l2, l3 := t.TempDir(), t.TempDir(), t.TempDir()
	w := func(d, p, s string) {
		os.MkdirAll(filepath.Dir(filepath.Join(d, p)), 0o755)
		os.WriteFile(filepath.Join(d, p), []byte(s), 0o644)
	}
	w(l1, "a/keep", "1")
	w(l1, "a/gone", "1")
	w(l1, "b/old", "1")
	w(l1, "c", "file")
	w(l1, "x/deep/f", "1")
	w(l2, "a/keep", "2") // override
	syscall.Mknod(filepath.Join(l2, "a/gone"), syscall.S_IFCHR, 0)
	os.MkdirAll(filepath.Join(l2, "b"), 0o755)
	syscall.Setxattr(filepath.Join(l2, "b"), opaqueXattr, []byte("y"), 0)
	w(l2, "b/new", "2")
	w(l3, "c/now-dir", "3") // file replaced by dir
	syscall.Mknod(filepath.Join(l3, "x"), syscall.S_IFCHR, 0)

	var buf bytes.Buffer
	if err := Flatten([]string{l1, l2, l3}, &buf); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		got[strings.TrimSuffix(h.Name, "/")] = string(b)
	}
	var names []string
	for k := range got {
		names = append(names, k)
	}
	sort.Strings(names)
	want := []string{"a", "a/keep", "b", "b/new", "c", "c/now-dir"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("flattened = %v, want %v", names, want)
	}
	if got["a/keep"] != "2" || got["c/now-dir"] != "3" {
		t.Errorf("wrong contents %v", got)
	}
}

func TestStoreLifecycle(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(content string) string {
		d, _ := s.TempDir("t-")
		os.WriteFile(filepath.Join(d, "f"), []byte(content), 0o644)
		id, err := s.CommitDir(d, "test")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	l1, l2 := mk("one"), mk("two")
	if dup := mk("one"); dup != l1 {
		t.Errorf("identical content produced a new layer")
	}
	d, _ := s.TempDir("empty-")
	if id, _ := s.CommitDir(d, ""); id != EmptyLayer {
		t.Errorf("empty dir -> %s", id)
	}
	img := &Image{Name: "app", Tag: "v1", Layers: []string{l1, l2}}
	if err := s.Save(img); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(&Image{Name: "bad", Tag: "x", Layers: []string{"sha256:missing"}}); err == nil {
		t.Error("saved image with missing layer")
	}
	got, err := s.Get("app:v1")
	if err != nil || got.ID != img.ID {
		t.Fatalf("get: %v %v", got, err)
	}
	if got, err := s.Get(Short(img.ID)[:8]); err != nil || got.ID != img.ID {
		t.Errorf("get by id prefix: %v", err)
	}
	if _, err := s.Tag("app:v1", "app:prod"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.List()
	if len(all) != 2 {
		t.Errorf("list = %d images", len(all))
	}
	s.CachePut("k", l2)
	if _, err := s.Remove("app:v1"); err != nil {
		t.Fatal(err)
	}
	res, _ := s.GC(nil)
	if len(res.Layers) != 0 {
		t.Errorf("GC removed layers still tagged app:prod: %v", res.Layers)
	}
	s.Remove("app:prod")
	res, _ = s.GC([]string{l1})
	if len(res.Layers) != 1 || res.Layers[0] != l2 {
		t.Errorf("GC = %v, want only %s", res.Layers, l2)
	}
	if _, ok := s.CacheGet("k"); ok {
		t.Error("cache entry for collected layer survived")
	}
}

func TestBundleRoundTripAndTamper(t *testing.T) {
	needRoot(t)
	s, _ := Open(t.TempDir())
	rootfs := mkTar(t, []tarFile{{name: "bin/", typ: tar.TypeDir, mode: 0o755}, {name: "bin/hello", typ: tar.TypeReg, body: "hello", mode: 0o755}}, true)
	img, err := s.ImportRootfs(bytes.NewReader(rootfs), "base:1", Config{Cmd: []string{"/bin/hello"}})
	if err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if err := s.SaveBundle(img, &bundle); err != nil {
		t.Fatal(err)
	}
	s2, _ := Open(t.TempDir())
	got, err := s2.LoadBundle(bytes.NewReader(bundle.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != img.ID || !reflect.DeepEqual(got.Config.Cmd, img.Config.Cmd) {
		t.Errorf("loaded %+v", got)
	}

	// Rewrite the bundle with a modified layer but the original name.
	var forged bytes.Buffer
	tw := tar.NewWriter(&forged)
	tr := tar.NewReader(bytes.NewReader(bundle.Bytes()))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(tr)
		if strings.HasPrefix(h.Name, "layers/") {
			body = mkTar(t, []tarFile{{name: "bin/hello", typ: tar.TypeReg, body: "evil", mode: 0o755}}, true)
			h.Size = int64(len(body))
		}
		tw.WriteHeader(h)
		tw.Write(body)
	}
	tw.Close()
	s3, _ := Open(t.TempDir())
	if _, err := s3.LoadBundle(&forged); err == nil || !strings.Contains(err.Error(), "tampered") {
		t.Errorf("tampered bundle accepted: %v", err)
	}
}

// fakeRegistry serves one single-layer image behind bearer auth.
func fakeRegistry(t *testing.T) (*httptest.Server, []byte) {
	layer := mkTar(t, []tarFile{{name: "hello.txt", typ: tar.TypeReg, body: "from registry"}}, true)
	sum := func(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }
	cfg := []byte(`{"architecture":"` + runtime.GOARCH + `","os":"linux","config":{"Env":["PATH=/bin"],"Cmd":["/hello"]},"history":[{"created_by":"ADD hello"}]}`)
	man := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s","size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"%s","size":%d}]}`,
		mtOCIManifest, sum(cfg), len(cfg), sum(layer), len(layer)))
	index := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"%s","manifests":[{"digest":"%s","platform":{"os":"linux","architecture":"%s"}}]}`,
		mtOCIIndex, sum(man), runtime.GOARCH))
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"token":"secret"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="test"`)
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/v2/team/app/manifests/v1":
			w.Header().Set("Content-Type", mtOCIIndex)
			w.Write(index)
		case "/v2/team/app/manifests/" + sum(man):
			w.Header().Set("Content-Type", mtOCIManifest)
			w.Write(man)
		case "/v2/team/app/blobs/" + sum(cfg):
			w.Write(cfg)
		case "/v2/team/app/blobs/" + sum(layer):
			w.Write(layer)
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, layer
}

func TestPullFromRegistry(t *testing.T) {
	srv, _ := fakeRegistry(t)
	defer srv.Close()
	s, _ := Open(t.TempDir())
	host := strings.TrimPrefix(srv.URL, "https://")
	img, err := s.Pull(context.Background(), host+"/team/app:v1", PullOptions{Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if len(img.Layers) != 1 || img.Config.Cmd[0] != "/hello" {
		t.Fatalf("image %+v", img)
	}
	b, err := os.ReadFile(filepath.Join(s.LayerDir(img.Layers[0]), "hello.txt"))
	if err != nil || string(b) != "from registry" {
		t.Errorf("layer content %q %v", b, err)
	}
	// Second pull reuses the blob.
	var out bytes.Buffer
	if _, err := s.Pull(context.Background(), host+"/team/app:v1", PullOptions{Client: srv.Client(), Out: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already present") {
		t.Errorf("blob not reused:\n%s", out.String())
	}
}

func buildStress(t *testing.T, dst string) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	cmd := exec.Command("go", "build", "-o", dst, "./cmd/crt-stress")
	cmd.Dir = filepath.Join(filepath.Dir(file), "..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build crt-stress: %v\n%s", err, out)
	}
}

func TestBuildWithCache(t *testing.T) {
	needRoot(t)
	root := t.TempDir()
	s, _ := Open(root)
	rt := container.New(root)
	rt.CgroupParent = "crt-test"
	ctxDir := t.TempDir()
	buildStress(t, filepath.Join(ctxDir, "crt-stress"))
	os.MkdirAll(filepath.Join(ctxDir, "conf"), 0o755)
	os.WriteFile(filepath.Join(ctxDir, "conf", "app.conf"), []byte("x=1\n"), 0o644)
	bf := `FROM scratch
COPY crt-stress /bin/
COPY conf /etc/app
ENV GREETING=hi MSG="${GREETING} there"
WORKDIR /data
RUN ["crt-stress", "-write", "/data/made-by-run"]
RUN ["crt-stress", "-rm", "/etc/app/app.conf"]
RUN ["crt-stress", "-exit", "0"]
CMD ["crt-stress", "-env", "MSG"]
`
	b := &Builder{Store: s, Runtime: rt}
	var out bytes.Buffer
	img, err := b.Build(context.Background(), BuildOptions{Buildfile: strings.NewReader(bf), ContextDir: ctxDir, Tag: "demo:1", Out: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	// 2 COPY + 2 RUN with changes; the no-op RUN adds no layer.
	if len(img.Layers) != 4 {
		t.Fatalf("layers = %d\n%s", len(img.Layers), out.String())
	}
	if _, err := os.Stat(filepath.Join(s.LayerDir(img.Layers[2]), "data", "made-by-run")); err != nil {
		t.Error("RUN output missing from its layer")
	}
	if fi, err := os.Lstat(filepath.Join(s.LayerDir(img.Layers[3]), "etc", "app", "app.conf")); err != nil || !isWhiteout(fi) {
		t.Error("RUN delete did not produce a whiteout layer")
	}
	for _, junk := range []string{"proc", "sys", "dev", "etc/resolv.conf"} {
		if _, err := os.Lstat(filepath.Join(s.LayerDir(img.Layers[2]), junk)); err == nil {
			t.Errorf("mount artifact %s committed into layer", junk)
		}
	}
	if img.Config.WorkingDir != "/data" || !strings.Contains(strings.Join(img.Config.Env, ","), "MSG=hi there") {
		t.Errorf("config %+v", img.Config)
	}

	out.Reset()
	img2, err := b.Build(context.Background(), BuildOptions{Buildfile: strings.NewReader(bf), ContextDir: ctxDir, Tag: "demo:2", Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "Using cache"); n != 3 || img2.ID != img.ID {
		t.Errorf("rebuild: %d cache hits, same id %v\n%s", n, img2.ID == img.ID, out.String())
	}

	// Run the image: CMD sees ENV.
	dirs, _ := s.Dirs(img)
	var runOut bytes.Buffer
	res, err := rt.Run(context.Background(), &container.Spec{Layers: dirs, Args: img.Config.Cmd, Env: img.Config.Env, WorkDir: img.Config.WorkingDir},
		container.RunOpts{Stdout: &runOut, Stderr: &runOut, Remove: true})
	if err != nil || res.ExitCode != 0 || runOut.String() != "hi there\n" {
		t.Errorf("run built image: %v exit %d out %q", err, res.ExitCode, runOut.String())
	}

	// Failing RUN aborts the build.
	_, err = b.Build(context.Background(), BuildOptions{Buildfile: strings.NewReader("FROM demo:1\nRUN [\"crt-stress\", \"-exit\", \"7\"]\n"), Tag: "bad:1"})
	if err == nil || !strings.Contains(err.Error(), "exit code 7") {
		t.Errorf("failing RUN: %v", err)
	}

	// Commit + squash + export agree on content.
	sq, err := s.Squash(img, "demo:squashed")
	if err != nil || len(sq.Layers) != 1 {
		t.Fatalf("squash: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.LayerDir(sq.Layers[0]), "etc/app/app.conf")); err == nil {
		t.Error("squash resurrected a deleted file")
	}
	if _, err := os.Stat(filepath.Join(s.LayerDir(sq.Layers[0]), "data/made-by-run")); err != nil {
		t.Error("squash lost a file")
	}
}
