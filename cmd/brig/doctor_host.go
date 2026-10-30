package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"

	"github.com/brig-sh/brig/internal/runtime"
)

// minSubIDs is the range rootless containerd needs to map an image's users.
const minSubIDs = 65536

// hostOS and linuxHostChecks are seams. A test on any host forces the rows on or
// checks them one at a time, and healthyHost turns them off, because what the
// machine running the tests lacks is not the test's subject.
var (
	hostOS          = goruntime.GOOS
	linuxHostChecks = linuxHostRows
)

// fileCapsOf and userNamespaces are seams for the same reason.
var (
	fileCapsOf     = hasFileCaps
	userNamespaces = func() error {
		out, err := exec.Command("unshare", "-Ur", "true").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
)

// linuxHostRows reports what a Linux run needs from the host. The installer of
// the runtime bundle checks most of these, which misses a host that took the
// release archive alone and a host that changed after the install. Without them
// doctor reads ok and every run fails. Findings only, as on the other rows:
// none of them changes the exit status.
//
// A run as root uses the node-wide install and maps no subordinate IDs.
func linuxHostRows(rt runtime.Runtime) []check {
	if hostOS != "linux" || (rt != nil && rt.Kind() != "nerdctl") {
		return nil
	}
	var rows []check
	if os.Geteuid() != 0 {
		rows = append(rows, subIDCheck("/etc/subuid", "/etc/subgid"), userNSCheck(), uidmapCheck())
	}
	rows = append(rows, devicesCheck("/dev/kvm", "/dev/vhost-vsock"), cniCheck())
	// The launcher sets BRIG_BOOT_ASSETS, and the bundle carries these tools.
	if os.Getenv("BRIG_BOOT_ASSETS") == "" {
		rows = append(rows, toolsCheck())
	}
	return rows
}

// subIDCount adds up the ranges a file gives this user, by name or by uid.
func subIDCount(path, name, uid string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	total := 0
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) != 3 || (f[0] != name && f[0] != uid) {
			continue
		}
		if n, err := strconv.Atoi(f[2]); err == nil && n > 0 {
			total += n
		}
	}
	return total
}

func subIDCheck(subuid, subgid string) check {
	me, err := user.Current()
	if err != nil {
		return check{Name: "subids", State: stateInfo, Finding: "cannot tell who this user is: " + err.Error()}
	}
	uids, gids := subIDCount(subuid, me.Username, me.Uid), subIDCount(subgid, me.Username, me.Uid)
	if uids < minSubIDs || gids < minSubIDs {
		return check{Name: "subids", State: stateFail,
			Finding: fmt.Sprintf("%s has %d subordinate uids and %d gids, and a rootless run needs %d of each", me.Username, uids, gids, minSubIDs),
			Fix:     fmt.Sprintf("sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 %s", me.Username),
		}
	}
	return check{Name: "subids", State: statePass, Finding: fmt.Sprintf("%d subordinate uids, %d gids", uids, gids)}
}

func userNSCheck() check {
	if err := userNamespaces(); err != nil {
		return check{Name: "userns", State: stateFail,
			Finding: "cannot create a user namespace: " + oneLine(err.Error()),
			Fix:     "allow unprivileged user namespaces: the kernel.unprivileged_userns_clone sysctl, or the rootlesskit AppArmor profile on Ubuntu",
		}
	}
	return check{Name: "userns", State: statePass, Finding: "this user can create a user namespace"}
}

func uidmapCheck() check {
	fix := "install uidmap (Debian, Ubuntu) or shadow (Arch, Fedora)"
	for _, name := range []string{"newuidmap", "newgidmap"} {
		path, err := exec.LookPath(name)
		if err != nil {
			return check{Name: "uidmap", State: stateFail, Finding: name + " is not on PATH", Fix: fix}
		}
		info, err := os.Stat(path)
		if err != nil || (info.Mode()&os.ModeSetuid == 0 && !fileCapsOf(path)) {
			return check{Name: "uidmap", State: stateFail,
				Finding: path + " is neither setuid nor carrying file capabilities", Fix: fix}
		}
	}
	return check{Name: "uidmap", State: statePass, Finding: "newuidmap and newgidmap are usable"}
}

func devicesCheck(devices ...string) check {
	for _, dev := range devices {
		f, err := os.OpenFile(dev, os.O_RDWR, 0)
		if err != nil {
			return check{Name: "devices", State: stateFail,
				Finding: dev + " could not be opened: " + oneLine(err.Error()),
				Fix:     "add yourself to the kvm group, and load the module: sudo modprobe vhost_vsock",
			}
		}
		_ = f.Close()
	}
	return check{Name: "devices", State: statePass, Finding: strings.Join(devices, " and ") + " are openable"}
}

var cniPathLine = regexp.MustCompile(`(?m)^\s*cni_path\s*=\s*"([^"]+)"`)

// cniDirs is where nerdctl looks for CNI plugins: CNI_PATH, or the cni_path of
// its configuration, or its default.
func cniDirs() []string {
	if v := os.Getenv("CNI_PATH"); v != "" {
		return filepath.SplitList(v)
	}
	config := os.Getenv("NERDCTL_TOML")
	if config == "" {
		config = "/etc/nerdctl/nerdctl.toml"
		if os.Geteuid() != 0 {
			if dir, err := os.UserConfigDir(); err == nil {
				config = filepath.Join(dir, "nerdctl", "nerdctl.toml")
			}
		}
	}
	if raw, err := os.ReadFile(config); err == nil {
		if m := cniPathLine.FindSubmatch(raw); m != nil {
			return []string{string(m[1])}
		}
	}
	return []string{"/opt/cni/bin"}
}

func cniCheck() check {
	dirs := cniDirs()
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "bridge")); err == nil {
			return check{Name: "cni", State: statePass, Finding: "the bridge plugin is in " + dir}
		}
	}
	return check{Name: "cni", State: stateFail,
		Finding: "no bridge plugin in " + strings.Join(dirs, ":") + ", and every isolated run needs it",
		Fix:     "run install.sh for the runtime bundle, or install the distro's CNI plugins",
	}
}

func toolsCheck() check {
	var missing []string
	for _, tool := range []string{"oras", "cloud-hypervisor"} {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return check{Name: "tools", State: stateFail,
			Finding: strings.Join(missing, " and ") + " not on PATH, and a genericBoot profile needs them",
			Fix:     "run install.sh for the runtime bundle",
		}
	}
	return check{Name: "tools", State: statePass, Finding: "oras and cloud-hypervisor are on PATH"}
}

// notTheBundle says when the runtime is not the bundle's, which is what a host
// that took the release archive alone has. The launcher always sets this
// variable, so an unset one means the launcher did not run.
func notTheBundle(rt runtime.Runtime) string {
	if hostOS != "linux" || rt == nil || rt.Kind() != "nerdctl" || os.Getenv("BRIG_RUNTIME_BIN") != "" {
		return ""
	}
	// The bundle line covers a brig that skipped its launcher, and that runtime
	// may well be the bundle's.
	if exe, err := doctorExecutable(); err == nil {
		if _, bypassed := runtime.MissingBundleEnv(exe); bypassed {
			return ""
		}
	}
	return " (not the runtime bundle; see install.sh)"
}
