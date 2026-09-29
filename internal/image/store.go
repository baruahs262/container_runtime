// Package image stores container images as stacks of content-addressed
// layer directories that overlayfs can mount directly, and provides the
// tools to build, pull, import, export and garbage-collect them.
//
// On-disk layout under the store root:
//
//	layers/<hex>/fs          layer contents, overlay-native whiteouts
//	layers/<hex>/layer.json  metadata
//	images/<ref>.json        image manifests (ref is path-escaped name:tag)
//	cache/<hex>              build cache: step key -> layer id
//	blobs/<hex>              registry blob digest -> layer id
//	tmp/                     staging area for atomic commits
package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// EmptyLayer marks a build step that changed no files.
const EmptyLayer = "empty"

// Config is the runtime configuration an image carries.
type Config struct {
	Env        []string          `json:"env,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	WorkingDir string            `json:"workdir,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// History records how each layer (or metadata change) came about.
type History struct {
	Created    time.Time `json:"created"`
	CreatedBy  string    `json:"created_by"`
	Layer      string    `json:"layer,omitempty"`
	EmptyLayer bool      `json:"empty_layer,omitempty"`
}

// Image is a named, ordered stack of layers plus configuration.
type Image struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Tag     string    `json:"tag"`
	Layers  []string  `json:"layers"` // layer ids, bottom-most first
	Config  Config    `json:"config"`
	History []History `json:"history,omitempty"`
	Created time.Time `json:"created"`
	Source  string    `json:"source,omitempty"`
}

// Ref returns name:tag.
func (i *Image) Ref() string { return i.Name + ":" + i.Tag }

// ComputeID derives the image id from its layers and config.
func (i *Image) ComputeID() string {
	b, _ := json.Marshal(struct {
		L []string
		C Config
	}{i.Layers, i.Config})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Layer is metadata for one stored layer.
type Layer struct {
	ID      string    `json:"id"`
	Size    int64     `json:"size"`
	Files   int       `json:"files"`
	Created time.Time `json:"created"`
	Source  string    `json:"source,omitempty"`
}

// Store is a directory holding layers and images.
type Store struct {
	Root string
}

// Open prepares a store rooted at root.
func Open(root string) (*Store, error) {
	s := &Store{Root: root}
	for _, d := range []string{"layers", "images", "cache", "blobs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func hexOf(id string) string { return strings.TrimPrefix(id, "sha256:") }

// LayerPath is the directory holding a layer's files and metadata.
func (s *Store) LayerPath(id string) string {
	return filepath.Join(s.Root, "layers", hexOf(id))
}

// LayerDir is the mountable contents of a layer.
func (s *Store) LayerDir(id string) string { return filepath.Join(s.LayerPath(id), "fs") }

// HasLayer reports whether a layer is present.
func (s *Store) HasLayer(id string) bool {
	_, err := os.Stat(filepath.Join(s.LayerPath(id), "layer.json"))
	return err == nil
}

// GetLayer loads a layer's metadata.
func (s *Store) GetLayer(id string) (*Layer, error) {
	b, err := os.ReadFile(filepath.Join(s.LayerPath(id), "layer.json"))
	if err != nil {
		return nil, fmt.Errorf("layer %s: %w", short(id), err)
	}
	var l Layer
	return &l, json.Unmarshal(b, &l)
}

// TempDir makes a staging directory on the same filesystem as the store,
// so commits are a rename.
func (s *Store) TempDir(prefix string) (string, error) {
	return os.MkdirTemp(filepath.Join(s.Root, "tmp"), prefix)
}

// CommitDir turns dir into a layer. It content-hashes dir, and either
// moves it into place or, if an identical layer already exists, deletes
// it. dir must be on the store's filesystem. An empty dir yields
// EmptyLayer.
func (s *Store) CommitDir(dir, source string) (string, error) {
	sum, err := TreeHash(dir)
	if err != nil {
		return "", err
	}
	if sum.Files == 0 {
		os.RemoveAll(dir)
		return EmptyLayer, nil
	}
	id := sum.ID
	if s.HasLayer(id) {
		os.RemoveAll(dir)
		return id, nil
	}
	stage, err := s.TempDir("layer-")
	if err != nil {
		return "", err
	}
	if err := os.Rename(dir, filepath.Join(stage, "fs")); err != nil {
		os.RemoveAll(stage)
		return "", fmt.Errorf("commit layer: %w", err)
	}
	meta := Layer{ID: id, Size: sum.Bytes, Files: sum.Files, Created: time.Now().UTC(), Source: source}
	b, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, "layer.json"), b, 0o644); err != nil {
		os.RemoveAll(stage)
		return "", err
	}
	if err := os.Rename(stage, s.LayerPath(id)); err != nil {
		os.RemoveAll(stage)
		if s.HasLayer(id) { // lost a race to an identical commit
			return id, nil
		}
		return "", err
	}
	return id, nil
}

func (s *Store) imagePath(name, tag string) string {
	return filepath.Join(s.Root, "images", url.PathEscape(name+":"+tag)+".json")
}

// Save writes an image record under its name:tag, computing its id.
func (s *Store) Save(img *Image) error {
	for _, l := range img.Layers {
		if !s.HasLayer(l) {
			return fmt.Errorf("image %s references missing layer %s", img.Ref(), short(l))
		}
	}
	img.ID = img.ComputeID()
	if img.Created.IsZero() {
		img.Created = time.Now().UTC()
	}
	b, err := json.MarshalIndent(img, "", "  ")
	if err != nil {
		return err
	}
	p := s.imagePath(img.Name, img.Tag)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ErrNotFound is returned when no image matches a reference.
var ErrNotFound = errors.New("image not found")

// Get finds an image by name[:tag] or by (a prefix of) its id.
func (s *Store) Get(ref string) (*Image, error) {
	if r, err := ParseRef(ref); err == nil && r.Digest == "" {
		if img, err := s.load(s.imagePath(r.Name, r.Tag)); err == nil {
			return img, nil
		}
	}
	id := hexOf(ref)
	if len(id) >= 4 {
		all, _ := s.List()
		var hit *Image
		for _, img := range all {
			if strings.HasPrefix(hexOf(img.ID), id) {
				if hit != nil && hit.ID != img.ID {
					return nil, fmt.Errorf("ambiguous image id %q", ref)
				}
				hit = img
			}
		}
		if hit != nil {
			return hit, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
}

func (s *Store) load(p string) (*Image, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var img Image
	if err := json.Unmarshal(b, &img); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &img, nil
}

// List returns all images sorted by name:tag.
func (s *Store) List() ([]*Image, error) {
	ents, err := os.ReadDir(filepath.Join(s.Root, "images"))
	if err != nil {
		return nil, err
	}
	var out []*Image
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if img, err := s.load(filepath.Join(s.Root, "images", e.Name())); err == nil {
			out = append(out, img)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref() < out[j].Ref() })
	return out, nil
}

// Tag gives an existing image an additional name.
func (s *Store) Tag(src, dst string) (*Image, error) {
	img, err := s.Get(src)
	if err != nil {
		return nil, err
	}
	r, err := ParseRef(dst)
	if err != nil {
		return nil, err
	}
	cp := *img
	cp.Name, cp.Tag = r.Name, r.Tag
	return &cp, s.Save(&cp)
}

// Remove deletes the name:tag record. Layers stay until GC.
func (s *Store) Remove(ref string) (*Image, error) {
	img, err := s.Get(ref)
	if err != nil {
		return nil, err
	}
	return img, os.Remove(s.imagePath(img.Name, img.Tag))
}

// Dirs resolves an image's layers to mountable directories.
func (s *Store) Dirs(img *Image) ([]string, error) {
	dirs := make([]string, len(img.Layers))
	for i, l := range img.Layers {
		if !s.HasLayer(l) {
			return nil, fmt.Errorf("image %s: missing layer %s", img.Ref(), short(l))
		}
		dirs[i] = s.LayerDir(l)
	}
	return dirs, nil
}

// CacheGet looks up a build-step key.
func (s *Store) CacheGet(key string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(s.Root, "cache", key))
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(string(b))
	if id != EmptyLayer && !s.HasLayer(id) {
		return "", false
	}
	return id, true
}

// CachePut records a build-step result.
func (s *Store) CachePut(key, layer string) error {
	return os.WriteFile(filepath.Join(s.Root, "cache", key), []byte(layer), 0o644)
}

func (s *Store) blobGet(digest string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(s.Root, "blobs", hexOf(digest)))
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(string(b))
	return id, s.HasLayer(id)
}

func (s *Store) blobPut(digest, layer string) error {
	return os.WriteFile(filepath.Join(s.Root, "blobs", hexOf(digest)), []byte(layer), 0o644)
}

// GCResult reports what garbage collection removed.
type GCResult struct {
	Layers []string
	Bytes  int64
}

// GC deletes layers that no image references and that are not in keep
// (layers used by containers), plus stale cache and staging entries.
func (s *Store) GC(keep []string) (*GCResult, error) {
	live := map[string]bool{}
	for _, l := range keep {
		live[hexOf(l)] = true
	}
	imgs, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, img := range imgs {
		for _, l := range img.Layers {
			live[hexOf(l)] = true
		}
	}
	res := &GCResult{}
	ents, _ := os.ReadDir(filepath.Join(s.Root, "layers"))
	for _, e := range ents {
		if live[e.Name()] {
			continue
		}
		id := "sha256:" + e.Name()
		if l, err := s.GetLayer(id); err == nil {
			res.Bytes += l.Size
		}
		if err := os.RemoveAll(filepath.Join(s.Root, "layers", e.Name())); err != nil {
			return res, err
		}
		res.Layers = append(res.Layers, id)
	}
	for _, d := range []string{"cache", "blobs"} {
		ents, _ := os.ReadDir(filepath.Join(s.Root, d))
		for _, e := range ents {
			p := filepath.Join(s.Root, d, e.Name())
			b, _ := os.ReadFile(p)
			id := strings.TrimSpace(string(b))
			if id != EmptyLayer && !s.HasLayer(id) {
				os.Remove(p)
			}
		}
	}
	tmp, _ := os.ReadDir(filepath.Join(s.Root, "tmp"))
	for _, e := range tmp {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
			os.RemoveAll(filepath.Join(s.Root, "tmp", e.Name()))
		}
	}
	return res, nil
}

// Short abbreviates an id for display.
func Short(id string) string { return short(id) }

func short(id string) string {
	h := hexOf(id)
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
