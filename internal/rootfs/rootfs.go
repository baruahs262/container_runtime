// Package rootfs assembles a container's filesystem view: it stacks image
// layers with overlayfs, populates /proc, /sys and a minimal /dev, applies
// bind mounts and path masking, and finally switches root with pivot_root
// or chroot.
//
// Every function here is meant to run inside the container's fresh mount
// namespace (the init process), never on the host.
package rootfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Bind is a host path mounted into the container.
type Bind struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readonly,omitempty"`
}

// Config is everything needed to build the root filesystem.
type Config struct {
	Layers   []string // lower directories, bottom-most first
	Upper    string   // writable overlay upper dir
	Work     string   // overlay work dir (same filesystem as Upper)
	Target   string   // mount point for the merged view
	Binds    []Bind
	Readonly bool   // mount the merged rootfs read-only
	HostNet  bool   // share host resolv.conf / hosts
	Empty    string // an empty dir used as lower when Layers is empty
}

// MakePrivate stops mount events propagating back to the host.
func MakePrivate() error {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	return nil
}

// OverlayOptions builds the mount data string. Overlay wants lowerdir
// top-most first. If the absolute option string would exceed a page, the
// caller should chdir into commonDir and use the returned relative form.
func OverlayOptions(layers []string, upper, work string) (opts string, commonDir string) {
	lower := make([]string, len(layers))
	for i, l := range layers {
		lower[len(layers)-1-i] = l
	}
	abs := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", strings.Join(lower, ":"), upper, work)
	if len(abs) < os.Getpagesize()-1 || len(lower) == 0 {
		return abs, ""
	}
	// Shorten by making every path relative to their longest common dir.
	common := lower[0]
	for _, p := range append(append([]string{}, lower...), upper, work) {
		for !strings.HasPrefix(p, common+"/") && common != "/" {
			common = filepath.Dir(common)
		}
	}
	rel := func(p string) string { r, _ := filepath.Rel(common, p); return r }
	for i := range lower {
		lower[i] = rel(lower[i])
	}
	return fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", strings.Join(lower, ":"), rel(upper), rel(work)), common
}

// MountOverlay stacks the layers at c.Target.
func MountOverlay(c *Config) error {
	layers := c.Layers
	if len(layers) == 0 {
		if c.Empty == "" {
			return errors.New("rootfs: no layers and no empty dir")
		}
		layers = []string{c.Empty}
	}
	for _, d := range []string{c.Upper, c.Work, c.Target} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	data, common := OverlayOptions(layers, c.Upper, c.Work)
	if len(data) >= os.Getpagesize() {
		return fmt.Errorf("rootfs: %d layers exceed overlay option limit", len(layers))
	}
	if common != "" {
		wd, _ := os.Getwd()
		if err := os.Chdir(common); err != nil {
			return err
		}
		defer os.Chdir(wd)
	}
	var flags uintptr
	if c.Readonly {
		flags |= syscall.MS_RDONLY
	}
	if err := syscall.Mount("overlay", c.Target, "overlay", flags, data); err != nil {
		return fmt.Errorf("mount overlay on %s: %w", c.Target, err)
	}
	return nil
}

type mnt struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
}

// devices are bind-mounted from the host; this works identically for
// privileged and user-namespaced containers and avoids mknod.
var devices = []string{"null", "zero", "full", "random", "urandom", "tty"}

// maskedPaths are hidden behind /dev/null (files) or an empty ro tmpfs
// (dirs); readonlyPaths are remounted read-only.
var (
	maskedPaths   = []string{"/proc/kcore", "/proc/keys", "/proc/timer_list", "/proc/sched_debug", "/proc/acpi", "/proc/scsi", "/sys/firmware"}
	readonlyPaths = []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"}
)

// Populate mounts the pseudo filesystems and binds under root (the merged
// overlay, before switching into it).
func Populate(root string, c *Config) error {
	const nsx = syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV
	mounts := []mnt{
		{"proc", "proc", "proc", nsx, ""},
		{"tmpfs", "dev", "tmpfs", syscall.MS_NOSUID | syscall.MS_STRICTATIME, "mode=755,size=65536k"},
		{"devpts", "dev/pts", "devpts", syscall.MS_NOSUID | syscall.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620"},
		{"shm", "dev/shm", "tmpfs", nsx, "mode=1777,size=65536k"},
		{"mqueue", "dev/mqueue", "mqueue", nsx, ""},
	}
	for _, m := range mounts {
		t := filepath.Join(root, m.target)
		if err := os.MkdirAll(t, 0o755); err != nil {
			return err
		}
		if err := syscall.Mount(m.source, t, m.fstype, m.flags, m.data); err != nil {
			if m.fstype == "mqueue" {
				continue // optional
			}
			return fmt.Errorf("mount %s on %s: %w", m.fstype, t, err)
		}
	}
	// sysfs can only be mounted fresh when we own a net namespace;
	// otherwise fall back to a read-only recursive bind of the host's.
	sys := filepath.Join(root, "sys")
	os.MkdirAll(sys, 0o755)
	if err := syscall.Mount("sysfs", sys, "sysfs", nsx|syscall.MS_RDONLY, ""); err != nil {
		if err := bind("/sys", sys, true, true); err != nil {
			return fmt.Errorf("mount sysfs: %w", err)
		}
	}
	for _, d := range devices {
		src := "/dev/" + d
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := bind(src, filepath.Join(root, "dev", d), false, false); err != nil {
			return err
		}
	}
	links := [][2]string{
		{"/proc/self/fd", "dev/fd"}, {"/proc/self/fd/0", "dev/stdin"},
		{"/proc/self/fd/1", "dev/stdout"}, {"/proc/self/fd/2", "dev/stderr"},
		{"pts/ptmx", "dev/ptmx"},
	}
	for _, l := range links {
		if err := os.Symlink(l[0], filepath.Join(root, l[1])); err != nil && !os.IsExist(err) {
			return err
		}
	}
	if c.HostNet {
		for _, f := range []string{"/etc/resolv.conf", "/etc/hosts"} {
			if _, err := os.Stat(f); err == nil {
				_ = bind(f, filepath.Join(root, f), true, false)
			}
		}
	}
	for _, b := range c.Binds {
		if err := bind(b.Source, filepath.Join(root, b.Target), b.ReadOnly, true); err != nil {
			return fmt.Errorf("bind %s:%s: %w", b.Source, b.Target, err)
		}
	}
	return nil
}

// bind mounts src onto dst, creating dst as a file or directory to match.
func bind(src, dst string, ro, recursive bool) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		err = os.MkdirAll(dst, 0o755)
	} else {
		if err = os.MkdirAll(filepath.Dir(dst), 0o755); err == nil {
			var f *os.File
			if f, err = os.OpenFile(dst, os.O_CREATE, 0o644); err == nil {
				f.Close()
			}
		}
	}
	if err != nil {
		return err
	}
	flags := uintptr(syscall.MS_BIND)
	if recursive {
		flags |= syscall.MS_REC
	}
	if err := syscall.Mount(src, dst, "", flags, ""); err != nil {
		return fmt.Errorf("bind %s -> %s: %w", src, dst, err)
	}
	if ro {
		// Read-only must be applied with a remount of the bind.
		if err := syscall.Mount("", dst, "", flags|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("remount ro %s: %w", dst, err)
		}
	}
	return nil
}

// Mask hides sensitive kernel interfaces. Call after switching root.
func Mask() error {
	for _, p := range maskedPaths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			err = syscall.Mount("tmpfs", p, "tmpfs", syscall.MS_RDONLY, "size=0")
		} else {
			err = syscall.Mount("/dev/null", p, "", syscall.MS_BIND, "")
		}
		if err != nil {
			return fmt.Errorf("mask %s: %w", p, err)
		}
	}
	for _, p := range readonlyPaths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := syscall.Mount(p, p, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("readonly %s: %w", p, err)
		}
		if err := syscall.Mount("", p, "", syscall.MS_BIND|syscall.MS_REC|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("readonly %s: %w", p, err)
		}
	}
	return nil
}

// PivotRoot makes root the new / and detaches the old root entirely, so
// nothing of the host filesystem stays reachable.
func PivotRoot(root string) error {
	if err := os.Chdir(root); err != nil {
		return err
	}
	// pivot_root(".", ".") stacks the old root on top of the new one at
	// the same place; unmounting "." then drops the old root.
	if err := syscall.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	return os.Chdir("/")
}

// Chroot changes the apparent root. Unlike PivotRoot, the host filesystem
// stays mounted in the namespace; a process holding CAP_SYS_CHROOT can
// escape, so this mode trades isolation for simplicity.
func Chroot(root string) error {
	if err := os.Chdir(root); err != nil {
		return err
	}
	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	return os.Chdir("/")
}
