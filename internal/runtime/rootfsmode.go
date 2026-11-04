package runtime

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The layout of a generic boot's guest root.
//
// An overlay root is the default where the host can build one: a read-only
// ext4 image of the image, built once per image, under a sparse 16 GiB ext4
// upper of the sandbox's own. hull reads the mode as HULL_ROOTFS_MODE and
// urunc as the rootfsMode annotation. Where the host cannot build it, brig
// passes no mode and the runtime boots its own root, virtio-fs on both.
// BRIG_ROOTFS_MODE=overlay-block insists on the overlay, and virtiofs asks for
// the runtime's own root.

// RootfsModeOverlayBlock boots the guest root from two disks: a read-only
// ext4 image of the image, built once per image, and a writable ext4 upper
// per sandbox.
const RootfsModeOverlayBlock = "overlay-block"

// RootfsModeVirtiofs keeps the runtime's own root. brig passes no mode for it,
// so the runtime boots the root it boots without one.
const RootfsModeVirtiofs = "virtiofs"

// OverlayRoot is whether the runtime on this host can build an overlay root.
// Why says what is missing when it cannot, in words that name the fix.
type OverlayRoot struct {
	Available bool
	Why       string
}

// rootfsModeFor is the rootfs mode brig hands the runtime for spec, or "" for
// the runtime's own root. It is the one place the default is decided, for the
// boot, the check and the ISOLATION row alike.
//
// spec.RootfsMode is BRIG_ROOTFS_MODE as set. Unset takes the overlay when
// three things hold: the profile boots an ordinary image with a rootfs type of
// virtiofs or none, the monitor takes an overlay root, and the host can build
// one. Otherwise brig passes no mode, and the runtime boots its own root.
// overlay-block asks for the overlay and is refused where the profile or the
// host cannot have it. Whether the monitor takes it is then the runtime's to
// say, as it was before the overlay was the default. virtiofs keeps the
// runtime's own root.
//
// monitor reports that the monitor takes an overlay root and that nothing
// else on the run already chose the layout of the root.
func rootfsModeFor(spec RunSpec, monitor bool, host OverlayRoot) (string, error) {
	asked := spec.RootfsMode
	switch asked {
	case "", RootfsModeOverlayBlock:
	case RootfsModeVirtiofs:
		return "", nil
	default:
		return "", fmt.Errorf("BRIG_ROOTFS_MODE=%q is not a rootfs mode: use %s or %s, or unset it for %s where this host can build it",
			asked, RootfsModeOverlayBlock, RootfsModeVirtiofs, RootfsModeOverlayBlock)
	}
	explicit := asked != ""
	if !spec.GenericBoot {
		if explicit {
			return "", fmt.Errorf("BRIG_ROOTFS_MODE=%s needs a profile that boots an ordinary image (genericBoot)", asked)
		}
		return "", nil
	}
	switch spec.RootfsType {
	case "", RootfsModeVirtiofs:
	default:
		if explicit {
			return "", fmt.Errorf("BRIG_ROOTFS_MODE=%s does not combine with rootfs type %s (BRIG_ROOTFS_TYPE or the profile's rootfsType): "+
				"the overlay root replaces it", asked, spec.RootfsType)
		}
		return "", nil
	}
	if !host.Available {
		if explicit {
			return "", fmt.Errorf("BRIG_ROOTFS_MODE=%s, but this host cannot build an overlay root: %s. "+
				"Unset BRIG_ROOTFS_MODE to boot on the runtime's own root", asked, host.Why)
		}
		return "", nil
	}
	if !explicit && !monitor {
		return "", nil
	}
	return RootfsModeOverlayBlock, nil
}

// OverlayRootFor reports whether rt can build an overlay root on this host,
// for brig doctor. A runtime brig does not drive itself builds none.
func OverlayRootFor(rt Runtime) OverlayRoot {
	switch r := rt.(type) {
	case *hull:
		return hullOverlayRoot()
	case *nerdctl:
		return r.overlayRoot()
	}
	return OverlayRoot{Why: "brig builds one on hull and on nerdctl with urunc only"}
}

// hullMke2fs is the mke2fs hull formats the overlay disks with. hull runs this
// path and no other (cmd/hull/blockrootfs.go), so it is the one checked. A var
// for tests.
var hullMke2fs = "/opt/homebrew/opt/e2fsprogs/sbin/mke2fs"

// hullOverlayRoot is whether hull can build an overlay root here: it can when
// its mke2fs is there to run.
func hullOverlayRoot() OverlayRoot {
	if !isExecutable(hullMke2fs) {
		return OverlayRoot{Why: "install e2fsprogs (brew install e2fsprogs): hull formats the disks with " + hullMke2fs}
	}
	return OverlayRoot{Available: true}
}

// OverlayMonitorFor reports whether a generic boot asking for hv under rt
// runs on a monitor that takes the default overlay root, and why not when it
// does not, for brig doctor. It is the monitor half of the run's decision. A
// runtime brig does not drive itself reports false with no reason.
func OverlayMonitorFor(rt Runtime, hv string) (bool, string) {
	switch r := rt.(type) {
	case *hull:
		return hullOverlayMonitor(hypervisorOrDefault(hv))
	case *nerdctl:
		return r.overlayMonitor(RunSpec{GenericBoot: true, Hypervisor: hv})
	}
	return false, ""
}

// hullOverlayMonitor is whether hull takes the default overlay root on hv.
// vz and hvi take one. HULL_ROOTFS_MODE, which hull inherits, and
// HULL_HVI_ROOTFS_UPPER choose the layout of the root themselves, and hull
// refuses the second next to an overlay, so either keeps the default off.
func hullOverlayMonitor(hv string) (bool, string) {
	switch {
	case hv != "vz" && hv != "hvi":
		return false, "hull's " + hv + " backend takes no overlay root"
	case os.Getenv("HULL_ROOTFS_MODE") != "":
		return false, "HULL_ROOTFS_MODE is set and chooses the root itself"
	case os.Getenv("HULL_HVI_ROOTFS_UPPER") != "":
		return false, "HULL_HVI_ROOTFS_UPPER is set and chooses the root itself"
	}
	return true, ""
}

// hullRootfsMode is the rootfs mode hull is handed on hv.
func hullRootfsMode(spec RunSpec, hv string) (string, error) {
	monitor, _ := hullOverlayMonitor(hv)
	return rootfsModeFor(spec, monitor, hullOverlayRoot())
}

// uruncOverlayMonitors are the urunc monitors that boot an overlay root.
var uruncOverlayMonitors = map[string]bool{"hvi": true, "cloud-hypervisor": true, "qemu": true}

// overlayRoot is whether urunc can build an overlay root under this driver.
// The rootfs mode is a urunc annotation, which docker does not carry and
// another shim does not read. urunc builds the disks from an overlayfs
// snapshot, and refuses a container from any other snapshotter, and it formats
// them with mke2fs.
func (n *nerdctl) overlayRoot() OverlayRoot {
	if n.isDocker() {
		return OverlayRoot{Why: "docker does not carry the rootfs mode to urunc; use nerdctl"}
	}
	if shim := containerdRuntime(); !isUruncShim(shim) {
		return OverlayRoot{Why: fmt.Sprintf("BRIG_CONTAINERD_RUNTIME=%s is not urunc, which builds the overlay root", shim)}
	}
	if snap, from := containerdSnapshotter(); snap != "overlayfs" {
		return OverlayRoot{Why: fmt.Sprintf("the containerd snapshotter is %s (%s), and urunc builds the overlay root from overlayfs only", snap, from)}
	}
	if !lookMke2fs() {
		return OverlayRoot{Why: "install e2fsprogs: urunc formats the disks with mke2fs, and there is none on PATH or in /usr/sbin"}
	}
	return OverlayRoot{Available: true}
}

// rootfsMode is the rootfs mode brig hands urunc in the annotation, or "" when
// it hands none. Like monitor, only a generic boot on nerdctl carries one. A
// monitor urunc does not run is CanRun's to refuse, and takes no default here.
func (n *nerdctl) rootfsMode(spec RunSpec) (string, error) {
	monitor, _ := n.overlayMonitor(spec)
	return rootfsModeFor(spec, monitor, n.overlayRoot())
}

// overlayMonitor is whether the urunc monitor spec runs on takes the overlay
// root, and why not when it does not.
func (n *nerdctl) overlayMonitor(spec RunSpec) (bool, string) {
	m, err := n.monitor(spec)
	switch {
	case err != nil:
		return false, err.Error()
	case !uruncOverlayMonitors[m]:
		return false, "urunc's " + orDefault(m, "default") + " monitor takes no overlay root"
	}
	return true, ""
}

// isUruncShim reports whether a BRIG_CONTAINERD_RUNTIME value is a runtime
// name containerd resolves to the urunc shim. See shimBoundary.
func isUruncShim(shim string) bool {
	return strings.HasPrefix(shim, "io.containerd.") && shimBinary(shim) == uruncShimBinary
}

// containerdSnapshotter is the snapshotter nerdctl unpacks into, and where
// that answer came from. nerdctl takes it from CONTAINERD_SNAPSHOTTER, then
// from the snapshotter key of its nerdctl.toml, and uses overlayfs when
// neither names one. brig passes no --snapshotter. The runtime bundle sets
// both the variable and the file, to overlayfs unless the user switched.
func containerdSnapshotter() (snapshotter, from string) {
	if v := os.Getenv("CONTAINERD_SNAPSHOTTER"); v != "" {
		return v, "CONTAINERD_SNAPSHOTTER"
	}
	path := nerdctlToml()
	if v := tomlSnapshotter(path); v != "" {
		return v, path
	}
	return "overlayfs", "nerdctl's default"
}

// nerdctlToml is the config file nerdctl reads: NERDCTL_TOML, else
// /etc/nerdctl/nerdctl.toml for root and the one under the user's config
// directory for anyone else, as nerdctl decides it.
func nerdctlToml() string {
	if v := os.Getenv("NERDCTL_TOML"); v != "" {
		return v
	}
	if os.Geteuid() == 0 {
		return "/etc/nerdctl/nerdctl.toml"
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "nerdctl", "nerdctl.toml")
}

// tomlSnapshotter reads the top-level snapshotter key of a nerdctl.toml, or ""
// when the file or the key is not there. The key lives above the first table,
// and the value is a plain string, so a line reader is all it takes.
func tomlSnapshotter(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "[") {
			return ""
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "snapshotter" {
			continue
		}
		val = strings.TrimSpace(val)
		if val != "" && (val[0] == '"' || val[0] == '\'') {
			if end := strings.IndexByte(val[1:], val[0]); end >= 0 {
				return val[1 : end+1]
			}
			return ""
		}
		val, _, _ = strings.Cut(val, "#")
		return strings.TrimSpace(val)
	}
	return ""
}

// lookMke2fs reports whether this host has the mke2fs urunc runs. urunc's
// default is /usr/sbin/mke2fs, and a PATH without /usr/sbin, which is a normal
// user's PATH on some distributions, does not mean the host has none. A var
// for tests.
var lookMke2fs = func() bool {
	if _, err := exec.LookPath("mke2fs"); err == nil {
		return true
	}
	return isExecutable("/usr/sbin/mke2fs") || isExecutable("/sbin/mke2fs")
}

// isExecutable reports whether path is a regular file someone may execute.
func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
