package image

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Bundle format written by SaveBundle and read by LoadBundle: a tar whose
// first entry is image.json, followed by one layers/<hex>.tar.gz per
// layer in stack order. Layer tarballs use OCI whiteouts, so a bundle is
// a self-contained, host-independent way to ship an image.
const bundleManifest = "image.json"

// SaveBundle writes img and all its layers to w.
func (s *Store) SaveBundle(img *Image, w io.Writer) error {
	tw := tar.NewWriter(w)
	b, err := json.MarshalIndent(img, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: bundleManifest, Mode: 0o644, Size: int64(len(b)), ModTime: img.Created}); err != nil {
		return err
	}
	if _, err := tw.Write(b); err != nil {
		return err
	}
	for _, l := range img.Layers {
		// Layers go through a temp file because tar needs sizes up front.
		f, err := os.CreateTemp(filepath.Join(s.Root, "tmp"), "save-")
		if err != nil {
			return err
		}
		gz := gzip.NewWriter(f)
		if err := WriteLayerTar(s.LayerDir(l), gz); err != nil {
			f.Close()
			os.Remove(f.Name())
			return err
		}
		gz.Close()
		size, _ := f.Seek(0, io.SeekCurrent)
		f.Seek(0, io.SeekStart)
		err = tw.WriteHeader(&tar.Header{Name: "layers/" + hexOf(l) + ".tar.gz", Mode: 0o644, Size: size, ModTime: time.Unix(0, 0)})
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		f.Close()
		os.Remove(f.Name())
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// LoadBundle imports an image written by SaveBundle, verifying that every
// layer hashes to the id recorded for it.
func (s *Store) LoadBundle(r io.Reader) (*Image, error) {
	tr := tar.NewReader(r)
	var img *Image
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch {
		case hdr.Name == bundleManifest:
			img = &Image{}
			if err := json.NewDecoder(tr).Decode(img); err != nil {
				return nil, fmt.Errorf("bundle manifest: %w", err)
			}
		case strings.HasPrefix(hdr.Name, "layers/") && strings.HasSuffix(hdr.Name, ".tar.gz"):
			if img == nil {
				return nil, fmt.Errorf("bundle: layer before %s", bundleManifest)
			}
			want := "sha256:" + strings.TrimSuffix(strings.TrimPrefix(hdr.Name, "layers/"), ".tar.gz")
			if s.HasLayer(want) {
				continue
			}
			got, err := s.importLayer(tr, "load bundle")
			if err != nil {
				return nil, err
			}
			if got != want {
				return nil, fmt.Errorf("bundle layer %s hashes to %s: corrupt or tampered", short(want), short(got))
			}
		}
	}
	if img == nil {
		return nil, fmt.Errorf("bundle has no %s", bundleManifest)
	}
	if err := s.Save(img); err != nil {
		return nil, err
	}
	return img, nil
}

// importLayer extracts a (possibly gzipped) layer tarball and commits it.
func (s *Store) importLayer(r io.Reader, source string) (string, error) {
	stage, err := s.TempDir("import-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	dir := filepath.Join(stage, "fs")
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := ExtractAuto(r, dir); err != nil {
		return "", err
	}
	return s.CommitDir(dir, source)
}

// ImportRootfs creates a single-layer image from a root filesystem
// tarball (plain or gzipped), e.g. an Alpine minirootfs.
func (s *Store) ImportRootfs(r io.Reader, ref string, cfg Config) (*Image, error) {
	pr, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	id, err := s.importLayer(r, "import "+ref)
	if err != nil {
		return nil, err
	}
	img := &Image{Name: pr.Name, Tag: pr.Tag, Config: cfg, Source: "import", Created: time.Now().UTC()}
	if id != EmptyLayer {
		img.Layers = []string{id}
		img.History = []History{{Created: img.Created, CreatedBy: "import " + ref, Layer: id}}
	}
	return img, s.Save(img)
}

// ExportRootfs writes the image's merged filesystem as one tarball.
func (s *Store) ExportRootfs(img *Image, w io.Writer) error {
	dirs, err := s.Dirs(img)
	if err != nil {
		return err
	}
	return Flatten(dirs, w)
}

// Squash collapses an image into a single layer saved under ref.
func (s *Store) Squash(img *Image, ref string) (*Image, error) {
	pr, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	pr2, pw := io.Pipe()
	go func() { pw.CloseWithError(s.ExportRootfs(img, pw)) }()
	id, err := s.importLayer(pr2, "squash "+img.Ref())
	pr2.CloseWithError(err)
	if err != nil {
		return nil, err
	}
	out := &Image{Name: pr.Name, Tag: pr.Tag, Config: img.Config, Source: "squash " + img.Ref(), Created: time.Now().UTC()}
	if id != EmptyLayer {
		out.Layers = []string{id}
		out.History = []History{{Created: out.Created, CreatedBy: "squash " + img.Ref(), Layer: id}}
	}
	return out, s.Save(out)
}

// Commit turns a container's writable layer into a new image on top of
// base.
func (s *Store) Commit(base *Image, upper string, ref, comment string) (*Image, error) {
	pr, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	var lowers []string
	if base != nil {
		if lowers, err = s.Dirs(base); err != nil {
			return nil, err
		}
	}
	stage, err := s.TempDir("commit-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	dir := filepath.Join(stage, "fs")
	// Copy rather than move so the container keeps its state.
	if err := copyLayerDir(upper, dir); err != nil {
		return nil, err
	}
	if err := PruneUpper(dir, lowers); err != nil {
		return nil, err
	}
	id, err := s.CommitDir(dir, "commit")
	if err != nil {
		return nil, err
	}
	img := &Image{Name: pr.Name, Tag: pr.Tag, Source: "commit", Created: time.Now().UTC()}
	if base != nil {
		img.Layers = append(img.Layers, base.Layers...)
		img.Config = base.Config
		img.History = append(img.History, base.History...)
	}
	h := History{Created: img.Created, CreatedBy: "commit: " + comment}
	if id == EmptyLayer {
		h.EmptyLayer = true
	} else {
		img.Layers = append(img.Layers, id)
		h.Layer = id
	}
	img.History = append(img.History, h)
	return img, s.Save(img)
}

// copyLayerDir duplicates a layer dir exactly (whiteouts, xattrs,
// ownership) by round-tripping through the layer tar format.
func copyLayerDir(src, dst string) error {
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(WriteLayerTar(src, pw)) }()
	_, err := Extract(pr, dst)
	pr.CloseWithError(err)
	return err
}
