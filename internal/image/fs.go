package image

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// OCI whiteout markers in layer tarballs, and overlayfs' native forms.
const (
	whPrefix     = ".wh."
	whOpaque     = ".wh..wh..opq"
	opaqueXattr  = "trusted.overlay.opaque"
	overlayXattr = "trusted.overlay."
	capXattr     = "security.capability"
	paxXattr     = "SCHILY.xattr."
)

// isWhiteout reports whether fi is an overlay whiteout (char device 0:0).
func isWhiteout(fi fs.FileInfo) bool {
	if fi.Mode()&fs.ModeCharDevice == 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Rdev == 0
}

func getxattr(p, name string) string {
	buf := make([]byte, 256)
	n, err := syscall.Getxattr(p, name, buf)
	if err != nil || n <= 0 {
		return ""
	}
	return string(buf[:n])
}

func isOpaque(p string) bool { return getxattr(p, opaqueXattr) == "y" }

// TreeSum is the result of hashing a directory tree.
type TreeSum struct {
	ID    string
	Files int
	Bytes int64
}

// TreeHash content-addresses a layer directory. It covers paths, file
// types, permissions, ownership, file contents, link targets, device
// numbers, opaque markers and file capabilities, but not timestamps, so
// rebuilding identical content yields the same layer id.
func TreeHash(dir string) (TreeSum, error) {
	h := sha256.New()
	var sum TreeSum
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		st := fi.Sys().(*syscall.Stat_t)
		fmt.Fprintf(h, "%s\x00%o\x00%d\x00%d\x00", filepath.ToSlash(rel), st.Mode, st.Uid, st.Gid)
		sum.Files++
		switch {
		case fi.Mode().IsRegular():
			sum.Bytes += fi.Size()
			if err := hashFile(h, p); err != nil {
				return err
			}
			fmt.Fprintf(h, "\x00%s", getxattr(p, capXattr))
		case fi.Mode()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			io.WriteString(h, t)
		case fi.Mode()&(fs.ModeDevice|fs.ModeCharDevice) != 0:
			fmt.Fprintf(h, "%d", st.Rdev)
		case fi.IsDir():
			if isOpaque(p) {
				io.WriteString(h, "opaque")
			}
		}
		h.Write([]byte{'\n'})
		return nil
	})
	if err != nil {
		return sum, err
	}
	sum.ID = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return sum, nil
}

func hashFile(h hash.Hash, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	fh := sha256.New()
	if _, err := io.Copy(fh, f); err != nil {
		return err
	}
	h.Write(fh.Sum(nil))
	return nil
}

// safeJoin resolves name inside root and refuses paths that escape it,
// either lexically ("../") or through a symlink planted by an earlier
// entry in the same archive.
func safeJoin(root, name string) (string, error) {
	clean := path.Clean("/" + name)
	if clean == "/" {
		return root, nil
	}
	parts := strings.Split(clean[1:], "/")
	cur := root
	for _, p := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("tar entry %q traverses symlink %s", name, cur)
		}
	}
	return filepath.Join(root, filepath.FromSlash(clean[1:])), nil
}

// Extract unpacks an OCI/Docker layer tarball into dir, converting
// whiteout files into overlayfs whiteouts. It returns the number of
// entries written.
func Extract(r io.Reader, dir string) (int, error) {
	tr := tar.NewReader(r)
	type dirTime struct {
		p string
		t time.Time
	}
	var dirs []dirTime
	n := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, fmt.Errorf("read tar: %w", err)
		}
		target, err := safeJoin(dir, hdr.Name)
		if err != nil {
			return n, err
		}
		if target == dir {
			continue
		}
		base := filepath.Base(target)
		parent := filepath.Dir(target)
		if strings.HasPrefix(base, whPrefix) {
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return n, err
			}
			if base == whOpaque {
				if err := syscall.Setxattr(parent, opaqueXattr, []byte("y"), 0); err != nil {
					return n, fmt.Errorf("mark opaque %s: %w", parent, err)
				}
			} else {
				wh := filepath.Join(parent, strings.TrimPrefix(base, whPrefix))
				os.RemoveAll(wh)
				if err := syscall.Mknod(wh, syscall.S_IFCHR, 0); err != nil {
					return n, fmt.Errorf("whiteout %s: %w", wh, err)
				}
			}
			n++
			continue
		}
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return n, err
		}
		mode := hdr.FileInfo().Mode()
		if hdr.Typeflag != tar.TypeDir {
			if fi, err := os.Lstat(target); err == nil {
				if fi.IsDir() {
					os.RemoveAll(target)
				} else {
					os.Remove(target)
				}
			}
		} else if fi, err := os.Lstat(target); err == nil && !fi.IsDir() {
			os.Remove(target)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return n, err
			}
			dirs = append(dirs, dirTime{target, hdr.ModTime})
		case tar.TypeReg, tar.TypeRegA:
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return n, err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return n, fmt.Errorf("write %s: %w", hdr.Name, err)
			}
		case tar.TypeSymlink:
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return n, err
			}
		case tar.TypeLink:
			src, err := safeJoin(dir, hdr.Linkname)
			if err != nil {
				return n, err
			}
			if err := os.Link(src, target); err != nil {
				return n, fmt.Errorf("hardlink %s -> %s: %w", hdr.Name, hdr.Linkname, err)
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			m := uint32(hdr.Mode & 0o7777)
			switch hdr.Typeflag {
			case tar.TypeChar:
				m |= syscall.S_IFCHR
			case tar.TypeBlock:
				m |= syscall.S_IFBLK
			default:
				m |= syscall.S_IFIFO
			}
			dev := int((hdr.Devmajor&0xfff)<<8 | hdr.Devminor&0xff | (hdr.Devminor&^0xff)<<12)
			if err := syscall.Mknod(target, m, dev); err != nil {
				return n, fmt.Errorf("mknod %s: %w", hdr.Name, err)
			}
		case tar.TypeXGlobalHeader:
			continue
		default:
			continue // sockets and exotic types are skipped
		}
		n++
		if err := os.Lchown(target, hdr.Uid, hdr.Gid); err != nil && !errors.Is(err, syscall.EPERM) {
			return n, err
		}
		if hdr.Typeflag != tar.TypeSymlink && hdr.Typeflag != tar.TypeLink {
			// chmod after chown: chown clears setuid bits.
			if err := os.Chmod(target, mode.Perm()|mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
				return n, err
			}
			for k, v := range hdr.PAXRecords {
				if strings.HasPrefix(k, paxXattr) {
					_ = syscall.Setxattr(target, strings.TrimPrefix(k, paxXattr), []byte(v), 0)
				}
			}
			if hdr.Typeflag != tar.TypeDir {
				_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
			}
		}
	}
	// Directory times last, deepest first, since creating children bumps them.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chtimes(dirs[i].p, dirs[i].t, dirs[i].t)
	}
	return n, nil
}

// tarEntry writes one filesystem object to tw under name.
func tarEntry(tw *tar.Writer, p, name string, fi fs.FileInfo, links map[[2]uint64]string) error {
	var link string
	if fi.Mode()&fs.ModeSymlink != 0 {
		var err error
		if link, err = os.Readlink(p); err != nil {
			return err
		}
	}
	hdr, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return err
	}
	hdr.Name = name
	if fi.IsDir() {
		hdr.Name += "/"
	}
	st := fi.Sys().(*syscall.Stat_t)
	hdr.Uid, hdr.Gid = int(st.Uid), int(st.Gid)
	hdr.Uname, hdr.Gname = "", ""
	hdr.ModTime = fi.ModTime().Truncate(time.Second)
	hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
	hdr.Format = tar.FormatPAX
	if fi.Mode().IsRegular() {
		if cap := getxattr(p, capXattr); cap != "" {
			hdr.PAXRecords = map[string]string{paxXattr + capXattr: cap}
		}
		if links != nil && st.Nlink > 1 {
			key := [2]uint64{st.Dev, st.Ino}
			if first, ok := links[key]; ok {
				hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeLink, first, 0
			} else {
				links[key] = name
			}
		}
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.CopyN(tw, f, hdr.Size); err != nil {
			return err
		}
	}
	return nil
}

// WriteLayerTar serialises a layer directory as an OCI layer tarball,
// turning overlay whiteouts and opaque dirs back into .wh. entries.
func WriteLayerTar(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	links := map[[2]uint64]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if isWhiteout(fi) {
			wh := path.Join(path.Dir(name), whPrefix+path.Base(name))
			return tw.WriteHeader(&tar.Header{Name: wh, Typeflag: tar.TypeReg, Mode: 0o644, ModTime: time.Unix(0, 0)})
		}
		if err := tarEntry(tw, p, name, fi, links); err != nil {
			return err
		}
		if fi.IsDir() && isOpaque(p) {
			return tw.WriteHeader(&tar.Header{Name: name + "/" + whOpaque, Typeflag: tar.TypeReg, Mode: 0o644, ModTime: time.Unix(0, 0)})
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// Flatten merges layers (bottom-most first) exactly as overlayfs would
// present them and writes the result as a single plain tarball.
func Flatten(layers []string, w io.Writer) error {
	decided := map[string]bool{}
	hidden := map[string]bool{} // subtrees masked by an upper layer
	chosen := map[string]string{}
	blocked := func(rel string) bool {
		for p := path.Dir(rel); p != "."; p = path.Dir(p) {
			if hidden[p] {
				return true
			}
		}
		return false
	}
	for i := len(layers) - 1; i >= 0; i-- {
		root := layers[i]
		pending := map[string]bool{}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			if rel == "." {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if blocked(rel) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if decided[rel] {
				return nil
			}
			decided[rel] = true
			fi, err := d.Info()
			if err != nil {
				return err
			}
			if isWhiteout(fi) {
				pending[rel] = true
				return nil
			}
			chosen[rel] = root
			if !fi.IsDir() || isOpaque(p) {
				pending[rel] = true
			}
			return nil
		})
		if err != nil {
			return err
		}
		for k := range pending {
			hidden[k] = true
		}
	}
	names := make([]string, 0, len(chosen))
	for k := range chosen {
		names = append(names, k)
	}
	sort.Strings(names)
	tw := tar.NewWriter(w)
	for _, name := range names {
		p := filepath.Join(chosen[name], filepath.FromSlash(name))
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if err := tarEntry(tw, p, name, fi, nil); err != nil {
			return err
		}
	}
	return tw.Close()
}

// mountArtifacts are paths the runtime may create in a container's upper
// dir purely as mount points.
var mountArtifacts = []string{"etc/resolv.conf", "etc/hosts", "proc", "sys", "dev"}

// PruneUpper cleans a container's upper dir before it becomes a layer:
// it drops empty mount-point stubs the runtime created, strips overlay
// bookkeeping xattrs, and removes directories that were merely copied up
// without changes. extra lists additional mount targets (bind mounts).
func PruneUpper(upper string, lowers []string, extra ...string) error {
	inLower := func(rel string) (fs.FileInfo, bool) {
		for i := len(lowers) - 1; i >= 0; i-- {
			if fi, err := os.Lstat(filepath.Join(lowers[i], rel)); err == nil {
				if isWhiteout(fi) {
					return nil, false
				}
				return fi, true
			}
		}
		return nil, false
	}
	for _, rel := range append(append([]string{}, mountArtifacts...), extra...) {
		rel = strings.TrimPrefix(filepath.Clean("/"+rel), "/")
		p := filepath.Join(upper, rel)
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if _, ok := inLower(rel); ok {
			continue
		}
		if (fi.Mode().IsRegular() && fi.Size() == 0) || (fi.IsDir() && isEmptyDir(p)) {
			os.Remove(p)
		}
	}
	// Post-order walk so children are handled before their parents.
	var dirs []string
	err := filepath.WalkDir(upper, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink == 0 {
			stripOverlayXattrs(p)
		}
		if d.IsDir() && p != upper {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		p := dirs[i]
		if !isEmptyDir(p) || isOpaque(p) {
			continue
		}
		rel, _ := filepath.Rel(upper, p)
		lfi, ok := inLower(rel)
		if !ok || !lfi.IsDir() {
			continue
		}
		ufi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		us, ls := ufi.Sys().(*syscall.Stat_t), lfi.Sys().(*syscall.Stat_t)
		if us.Mode == ls.Mode && us.Uid == ls.Uid && us.Gid == ls.Gid {
			os.Remove(p)
		}
	}
	return nil
}

func isEmptyDir(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	names, _ := f.Readdirnames(1)
	return len(names) == 0
}

// stripOverlayXattrs removes overlay bookkeeping (origin, impure...) but
// keeps the opaque marker, which is part of the layer's meaning.
func stripOverlayXattrs(p string) {
	buf := make([]byte, 1024)
	n, err := syscall.Listxattr(p, buf)
	if err != nil || n <= 0 {
		return
	}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:n]), "\x00"), "\x00") {
		if strings.HasPrefix(name, overlayXattr) && name != opaqueXattr {
			_ = syscall.Removexattr(p, name)
		}
	}
}
