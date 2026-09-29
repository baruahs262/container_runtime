package image

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Manifest media types accepted from registries.
const (
	mtOCIIndex     = "application/vnd.oci.image.index.v1+json"
	mtOCIManifest  = "application/vnd.oci.image.manifest.v1+json"
	mtDockerList   = "application/vnd.docker.distribution.manifest.list.v2+json"
	mtDockerSchema = "application/vnd.docker.distribution.manifest.v2+json"
)

// PullOptions configures a registry pull.
type PullOptions struct {
	Client      *http.Client
	OS, Arch    string
	Concurrency int // parallel layer downloads
	Out         io.Writer
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform,omitempty"`
}

type manifest struct {
	MediaType string       `json:"mediaType"`
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
	Manifests []descriptor `json:"manifests"` // index / list
}

type ociConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Config       struct {
		Env        []string          `json:"Env"`
		Cmd        []string          `json:"Cmd"`
		Entrypoint []string          `json:"Entrypoint"`
		WorkingDir string            `json:"WorkingDir"`
		Labels     map[string]string `json:"Labels"`
	} `json:"config"`
	History []struct {
		Created    time.Time `json:"created"`
		CreatedBy  string    `json:"created_by"`
		EmptyLayer bool      `json:"empty_layer"`
	} `json:"history"`
}

// registryClient speaks the OCI distribution API with anonymous bearer
// token auth.
type registryClient struct {
	http  *http.Client
	host  string
	repo  string
	mu    sync.Mutex
	token string
}

// parseChallenge extracts parameters from a WWW-Authenticate header such
// as: Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"
func parseChallenge(h string) (scheme string, params map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params = map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		if strings.HasPrefix(v, `"`) {
			end := strings.Index(v[1:], `"`)
			if end < 0 {
				params[strings.ToLower(k)] = v[1:]
				break
			}
			params[strings.ToLower(k)] = v[1 : end+1]
			rest = v[end+2:]
		} else {
			val, r, _ := strings.Cut(v, ",")
			params[strings.ToLower(k)] = val
			rest = r
		}
	}
	return strings.ToLower(scheme), params
}

func (c *registryClient) authenticate(ctx context.Context, challenge string) error {
	scheme, p := parseChallenge(challenge)
	if scheme != "bearer" || p["realm"] == "" {
		return fmt.Errorf("registry %s requires unsupported auth %q", c.host, scheme)
	}
	u, err := url.Parse(p["realm"])
	if err != nil {
		return err
	}
	q := u.Query()
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	scope := p["scope"]
	if scope == "" {
		scope = "repository:" + c.repo + ":pull"
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("token request: %s", resp.Status)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return err
	}
	c.mu.Lock()
	c.token = tok.Token
	if c.token == "" {
		c.token = tok.AccessToken
	}
	c.mu.Unlock()
	return nil
}

func (c *registryClient) get(ctx context.Context, path string, accept ...string) (*http.Response, error) {
	u := "https://" + c.host + "/v2/" + c.repo + path
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		for _, a := range accept {
			req.Header.Add("Accept", a)
		}
		c.mu.Lock()
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		c.mu.Unlock()
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			ch := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			if err := c.authenticate(ctx, ch); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return nil, fmt.Errorf("GET %s: %s %s", path, resp.Status, strings.TrimSpace(string(b)))
		}
		return resp, nil
	}
	return nil, fmt.Errorf("GET %s: unauthorized", path)
}

func (c *registryClient) manifest(ctx context.Context, ref string) (*manifest, string, error) {
	resp, err := c.get(ctx, "/manifests/"+ref, mtOCIIndex, mtDockerList, mtOCIManifest, mtDockerSchema)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	dgst := "sha256:" + hex.EncodeToString(sum[:])
	if strings.HasPrefix(ref, "sha256:") && ref != dgst {
		return nil, "", fmt.Errorf("manifest digest mismatch: want %s got %s", ref, dgst)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, "", err
	}
	if m.MediaType == "" {
		m.MediaType = resp.Header.Get("Content-Type")
	}
	return &m, dgst, nil
}

// selectPlatform picks the manifest for os/arch from an index.
func selectPlatform(m *manifest, osName, arch string) (string, error) {
	var seen []string
	for _, d := range m.Manifests {
		if d.Platform == nil {
			continue
		}
		seen = append(seen, d.Platform.OS+"/"+d.Platform.Architecture)
		if d.Platform.OS == osName && d.Platform.Architecture == arch {
			return d.Digest, nil
		}
	}
	return "", fmt.Errorf("no %s/%s image in index (have %s)", osName, arch, strings.Join(seen, ", "))
}

// verifyingReader hashes everything read through it.
type verifyingReader struct {
	r    io.Reader
	h    hash.Hash
	want string
	n    int64
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	v.h.Write(p[:n])
	v.n += int64(n)
	return n, err
}

func (v *verifyingReader) verify() error {
	got := "sha256:" + hex.EncodeToString(v.h.Sum(nil))
	if got != v.want {
		return fmt.Errorf("blob digest mismatch: want %s got %s", v.want, got)
	}
	return nil
}

// Pull downloads an image from its registry into the store. Layers are
// fetched and unpacked concurrently; each blob is verified against its
// digest before it is committed.
func (s *Store) Pull(ctx context.Context, refStr string, o PullOptions) (*Image, error) {
	ref, err := ParseRef(refStr)
	if err != nil {
		return nil, err
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 10 * time.Minute}
	}
	if o.OS == "" {
		o.OS = "linux"
	}
	if o.Arch == "" {
		o.Arch = runtime.GOARCH
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	host, repo := ref.Registry()
	c := &registryClient{http: o.Client, host: host, repo: repo}
	target := ref.Tag
	if ref.Digest != "" {
		target = ref.Digest
	}
	fmt.Fprintf(o.Out, "Pulling %s from %s/%s\n", ref, host, repo)
	m, dgst, err := c.manifest(ctx, target)
	if err != nil {
		return nil, err
	}
	if len(m.Manifests) > 0 || m.MediaType == mtOCIIndex || m.MediaType == mtDockerList {
		d, err := selectPlatform(m, o.OS, o.Arch)
		if err != nil {
			return nil, err
		}
		if m, dgst, err = c.manifest(ctx, d); err != nil {
			return nil, err
		}
	}
	fmt.Fprintf(o.Out, "Digest: %s\n", dgst)

	// Config blob.
	resp, err := c.get(ctx, "/blobs/"+m.Config.Digest)
	if err != nil {
		return nil, err
	}
	vr := &verifyingReader{r: io.LimitReader(resp.Body, 16<<20), h: sha256.New(), want: m.Config.Digest}
	cb, err := io.ReadAll(vr)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if err := vr.verify(); err != nil {
		return nil, err
	}
	var cfg ociConfig
	if err := json.Unmarshal(cb, &cfg); err != nil {
		return nil, fmt.Errorf("image config: %w", err)
	}

	// Layers, concurrently.
	ids := make([]string, len(m.Layers))
	errs := make([]error, len(m.Layers))
	sem := make(chan struct{}, o.Concurrency)
	var wg sync.WaitGroup
	var outMu sync.Mutex
	logf := func(f string, a ...any) {
		outMu.Lock()
		fmt.Fprintf(o.Out, f, a...)
		outMu.Unlock()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i, l := range m.Layers {
		wg.Add(1)
		go func(i int, l descriptor) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if id, ok := s.blobGet(l.Digest); ok {
				logf("  %s: already present\n", short(l.Digest))
				ids[i] = id
				return
			}
			t0 := time.Now()
			id, err := s.pullLayer(ctx, c, l)
			if err != nil {
				errs[i] = fmt.Errorf("layer %s: %w", short(l.Digest), err)
				cancel()
				return
			}
			ids[i] = id
			logf("  %s: pulled %.1f MB in %s\n", short(l.Digest), float64(l.Size)/1e6, time.Since(t0).Round(time.Millisecond))
		}(i, l)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	img := &Image{
		Name: ref.Name, Tag: ref.Tag, Source: fmt.Sprintf("pull %s/%s@%s", host, repo, dgst),
		Config: Config{
			Env: cfg.Config.Env, Cmd: cfg.Config.Cmd, Entrypoint: cfg.Config.Entrypoint,
			WorkingDir: cfg.Config.WorkingDir, Labels: cfg.Config.Labels,
		},
		Created: time.Now().UTC(),
	}
	if img.Tag == "" {
		img.Tag = short(ref.Digest)
	}
	for _, id := range ids {
		if id != EmptyLayer {
			img.Layers = append(img.Layers, id)
		}
	}
	// Non-empty history entries correspond, in order, to the manifest's
	// layers (including ones that unpacked to nothing).
	li := 0
	for _, h := range cfg.History {
		e := History{Created: h.Created, CreatedBy: h.CreatedBy, EmptyLayer: h.EmptyLayer}
		if !h.EmptyLayer && li < len(ids) {
			if ids[li] == EmptyLayer {
				e.EmptyLayer = true
			} else {
				e.Layer = ids[li]
			}
			li++
		}
		img.History = append(img.History, e)
	}
	if err := s.Save(img); err != nil {
		return nil, err
	}
	fmt.Fprintf(o.Out, "Pulled %s (%s, %d layers)\n", img.Ref(), short(img.ID), len(img.Layers))
	return img, nil
}

func (s *Store) pullLayer(ctx context.Context, c *registryClient, l descriptor) (string, error) {
	if strings.HasSuffix(l.MediaType, "zstd") {
		return "", fmt.Errorf("zstd-compressed layers are not supported")
	}
	resp, err := c.get(ctx, "/blobs/"+l.Digest)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	vr := &verifyingReader{r: resp.Body, h: sha256.New(), want: l.Digest}
	stage, err := s.TempDir("pull-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	dir := stage + "/fs"
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := ExtractAuto(vr, dir); err != nil {
		return "", err
	}
	// Drain trailing padding so the digest covers the whole blob.
	if _, err := io.Copy(io.Discard, vr); err != nil {
		return "", err
	}
	if err := vr.verify(); err != nil {
		return "", err
	}
	id, err := s.CommitDir(dir, "pull "+c.host+"/"+c.repo+"@"+l.Digest)
	if err != nil {
		return "", err
	}
	return id, s.blobPut(l.Digest, id)
}

// ExtractAuto extracts a tar stream that may be gzip-compressed.
func ExtractAuto(r io.Reader, dir string) (int, error) {
	br := bufio.NewReader(r)
	magic, _ := br.Peek(2)
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return 0, err
		}
		defer gz.Close()
		n, err := Extract(gz, dir)
		if err != nil {
			return n, err
		}
		_, err = io.Copy(io.Discard, gz)
		return n, err
	}
	n, err := Extract(br, dir)
	if err != nil {
		return n, err
	}
	_, err = io.Copy(io.Discard, br)
	return n, err
}
