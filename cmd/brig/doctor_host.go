package main

import (
	"errors"
	"fmt"
	"io/fs"
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

// These are seams for the same reason.
var (
	geteuid        = os.Geteuid
	fileCapsOf     = hasFileCaps
	userNamespaces = func() error {
		out, err := exec.Command("unshare", "-Ur", "true").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	// apparmorRestrictsUserNS is Ubuntu's switch since 24.04, on by default.
	apparmorRestrictsUserNS = func() bool {
		raw, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
		return err == nil && strings.TrimSpace(string(raw)) == "1"
	}
	apparmorDir = "/etc/apparmor.d"
)

// bundleCarriesThem is the fix on a row whose usual advice is to install or
// fetch what the runtime bundle already has, when the bundle line says why this
// brig cannot see it.
const bundleCarriesThem = "the runtime bundle carries them; see the bundle line above"

// linuxHostRows reports what a Linux run needs from the host. The installer of
// the runtime bundle checks most of these, which misses a host that took the
// release archive alone and a host that changed after the install. Without them
// doctor reads ok and every run fails. Findings only, as on the other rows:
// none of them changes the exit status.
//
// A run as root uses the node-wide install and maps no subordinate IDs.
//
// hypervisor is what a run would ask for: BRIG_HYPERVISOR, or the agent's own
// field. The tools row looks for the monitor it picks.
func linuxHostRows(rt runtime.Runtime, hypervisor string) []check {
	if hostOS != "linux" || (rt != nil && rt.Kind() != "nerdctl") {
		return nil
	}
	var rows []check
	if geteuid() != 0 {
		rows = append(rows, subIDCheck("/etc/subuid", "/etc/subgid"), userNSCheck(rt), uidmapCheck())
	}
	// /dev/kvm has the virtual line already.
	rows = append(rows, devicesCheck("/dev/vhost-vsock"), cniCheck())
	// The bundle carries these tools. Its tree is the evidence and not a variable:
	// a host can name its own kernel or its own nerdctl and still lack them.
	if rt == nil || !runtime.InBundleTree(rt.Bin()) {
		// oras only fetches boot assets, so a host that has them does not need it.
		_, present, _, err := runtime.BootAssetsDir(rt)
		rows = append(rows, toolsCheck(err != nil || !present, hypervisor))
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

// nextSubIDStart is where a new range starts: past every range either file hands
// out, since a range that overlaps another user's maps both users' containers
// onto the same host uids. The bundle's installer offers the same one.
func nextSubIDStart(paths ...string) int {
	next := 100000
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			f := strings.Split(strings.TrimSpace(line), ":")
			if len(f) != 3 {
				continue
			}
			start, serr := strconv.Atoi(f[1])
			count, cerr := strconv.Atoi(f[2])
			if serr == nil && cerr == nil && start+count > next {
				next = start + count
			}
		}
	}
	return next
}

func subIDCheck(subuid, subgid string) check {
	me, err := user.Current()
	if err != nil {
		return check{Name: "subids", State: stateInfo, Finding: "cannot tell who this user is: " + err.Error()}
	}
	uids, gids := subIDCount(subuid, me.Username, me.Uid), subIDCount(subgid, me.Username, me.Uid)
	if uids < minSubIDs || gids < minSubIDs {
		start := nextSubIDStart(subuid, subgid)
		return check{Name: "subids", State: stateFail,
			Finding: fmt.Sprintf("%s has %d subordinate uids and %d gids, and a rootless run needs %d of each", me.Username, uids, gids, minSubIDs),
			Fix: fmt.Sprintf("sudo usermod --add-subuids %[1]d-%[2]d --add-subgids %[1]d-%[2]d %[3]s",
				start, start+minSubIDs-1, me.Username),
		}
	}
	return check{Name: "subids", State: statePass, Finding: fmt.Sprintf("%d subordinate uids, %d gids", uids, gids)}
}

// userNSCheck asks whether the rootless setup can create its user namespace.
//
// Where AppArmor restricts them, a probe from here fails on a host that is set up
// right: the profile that allows them names the rootlesskit binary, and covers
// nothing else. So that host is asked for the profile, as the bundle's installer
// asks.
func userNSCheck(rt runtime.Runtime) check {
	if apparmorRestrictsUserNS() {
		return rootlesskitProfileCheck(rootlesskitPath(rt))
	}
	err := userNamespaces()
	if errors.Is(err, exec.ErrNotFound) {
		return check{Name: "userns", State: stateInfo, Finding: "unshare is not on PATH, so doctor cannot try to create a user namespace"}
	}
	if err != nil {
		return check{Name: "userns", State: stateFail,
			Finding: "cannot create a user namespace: " + oneLine(err.Error()),
			Fix:     "allow unprivileged user namespaces: user.max_user_namespaces above 0, and kernel.unprivileged_userns_clone=1 on a kernel that has it",
		}
	}
	return check{Name: "userns", State: statePass, Finding: "this user can create a user namespace"}
}

// rootlesskitPath is the rootlesskit a run starts: the bundle's, beside its
// nerdctl, or the one on PATH. Empty when there is none.
func rootlesskitPath(rt runtime.Runtime) string {
	if rt != nil && runtime.InBundleTree(rt.Bin()) {
		return filepath.Join(filepath.Dir(rt.Bin()), "rootlesskit")
	}
	path, _ := exec.LookPath("rootlesskit")
	return path
}

func rootlesskitProfileCheck(rootlesskit string) check {
	if rootlesskit == "" {
		return check{Name: "userns", State: stateInfo,
			Finding: "AppArmor restricts user namespaces, and there is no rootlesskit to look for a profile of"}
	}
	if !apparmorNames(rootlesskit) {
		return check{Name: "userns", State: stateFail,
			Finding: "AppArmor restricts user namespaces, and no profile in " + apparmorDir + " names " + rootlesskit,
			Fix:     "run install.sh for the runtime bundle, which prints the profile for root to add",
		}
	}
	return check{Name: "userns", State: statePass, Finding: "AppArmor has a profile for " + rootlesskit}
}

// apparmorNames says whether a file under the profile directory names the path.
// A profile names the binary it covers by path.
func apparmorNames(path string) bool {
	found := false
	_ = filepath.WalkDir(apparmorDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found || !d.Type().IsRegular() {
			return nil
		}
		if raw, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(raw), path) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
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

func devicesCheck(dev string) check {
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		fix := "this user needs the device: run install.sh, whose rootless setup adds a udev rule with sudo, or ask root to grant it"
		if errors.Is(err, fs.ErrNotExist) {
			fix = "load the module: sudo modprobe vhost_vsock"
		}
		return check{Name: "devices", State: stateFail,
			Finding: dev + " could not be opened: " + oneLine(err.Error()), Fix: fix}
	}
	_ = f.Close()
	return check{Name: "devices", State: statePass, Finding: dev + " is openable"}
}

var cniPathLine = regexp.MustCompile(`(?m)^\s*cni_path\s*=\s*(?:"([^"]+)"|'([^']+)')`)

// cniDirs is where nerdctl looks for CNI plugins: CNI_PATH, or the cni_path of
// its configuration, or its default.
func cniDirs() []string {
	if v := os.Getenv("CNI_PATH"); v != "" {
		return filepath.SplitList(v)
	}
	config := os.Getenv("NERDCTL_TOML")
	if config == "" {
		config = "/etc/nerdctl/nerdctl.toml"
		if geteuid() != 0 {
			if dir, err := os.UserConfigDir(); err == nil {
				config = filepath.Join(dir, "nerdctl", "nerdctl.toml")
			}
		}
	}
	if raw, err := os.ReadFile(config); err == nil {
		if m := cniPathLine.FindSubmatch(raw); m != nil {
			return []string{string(m[1]) + string(m[2])}
		}
	}
	return []string{defaultCNIDir()}
}

// defaultCNIDir is nerdctl's defaults.CNIPath: the first of these directories
// that exists, with a rootless user's own ones ahead of the rest. A distro
// nerdctl with the distro's plugins uses /usr/lib/cni or /usr/libexec/cni, and
// a row that knew only /opt/cni/bin told that host to install what it had. It
// takes the first that exists and does not merge them, so neither does this.
func defaultCNIDir() string {
	candidates := []string{
		"/opt/cni/bin",
		"/usr/local/libexec/cni",
		"/usr/local/lib/cni",
		"/home/linuxbrew/.linuxbrew/opt/cni-plugins/bin",
		"/usr/libexec/cni",
		"/usr/lib/cni",
	}
	if geteuid() != 0 {
		if home := os.Getenv("HOME"); home != "" {
			candidates = append([]string{
				filepath.Join(home, "opt/cni/bin"),
				filepath.Join(home, ".local/libexec/cni"),
				filepath.Join(home, ".local/lib/cni"),
			}, candidates...)
		}
	}
	for _, dir := range candidates {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}
	return "/opt/cni/bin"
}

// cniPlugins are what a nerdctl bridge network calls: the chain of a brig sandbox
// network, its IPAM and the loopback. A directory with some of them passes
// nerdctl's own check on bridge and fails later.
var cniPlugins = []string{"bridge", "firewall", "host-local", "loopback", "portmap", "tuning"}

func cniCheck() check {
	dirs := cniDirs()
	var missing []string
	for _, plugin := range cniPlugins {
		found := false
		for _, dir := range dirs {
			if _, err := os.Stat(filepath.Join(dir, plugin)); err == nil {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, plugin)
		}
	}
	joined := strings.Join(dirs, ":")
	if len(missing) > 0 {
		return check{Name: "cni", State: stateFail,
			Finding: "no " + strings.Join(missing, ", ") + " plugin in " + joined + ", and every isolated run needs them",
			Fix:     "run install.sh for the runtime bundle, or install the distro's CNI plugins",
		}
	}
	return check{Name: "cni", State: statePass, Finding: "the CNI plugins are in " + joined}
}

// toolsCheck looks for the monitor binary urunc runs for this hypervisor. The
// default moved to hvi, and a row that still asked for cloud-hypervisor passed a
// host that cannot boot and failed one that can.
func toolsCheck(needOras bool, hypervisor string) check {
	monitor, err := runtime.LinuxMonitorBinary(hypervisor)
	if err != nil {
		return check{Name: "tools", State: stateFail, Finding: oneLine(err.Error()),
			Fix: "set BRIG_HYPERVISOR, or the profile's hypervisor field, to a monitor urunc runs"}
	}
	tools := []string{monitor}
	if needOras {
		tools = append([]string{"oras"}, tools...)
	}
	var missing []string
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		pronoun := "it"
		if len(missing) > 1 {
			pronoun = "them"
		}
		return check{Name: "tools", State: stateFail,
			Finding: strings.Join(missing, " and ") + " not on PATH, and a genericBoot run needs " + pronoun,
			Fix:     "run install.sh for the runtime bundle",
		}
	}
	return check{Name: "tools", State: statePass, Finding: strings.Join(tools, " and ") + " on PATH"}
}

// notTheBundle says when the runtime is not the bundle's, which is what a host
// that took the release archive alone has.
func notTheBundle(rt runtime.Runtime) string {
	if hostOS != "linux" || rt == nil || rt.Kind() != "nerdctl" || runtime.InBundleTree(rt.Bin()) {
		return ""
	}
	return " (not the runtime bundle; see install.sh)"
}
