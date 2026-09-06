package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageBootAssets writes the pair a genericBoot profile needs and points
// BRIG_BOOT_ASSETS at them, returning the paths the annotations should carry.
func stageBootAssets(t *testing.T) (kernel, initrd string) {
	t.Helper()
	dir := t.TempDir()
	kernel = filepath.Join(dir, bootKernelName())
	initrd = filepath.Join(dir, bootInitrdName)
	for _, p := range []string{kernel, initrd} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BRIG_BOOT_ASSETS", dir)
	return kernel, initrd
}

// urunc reads the same two annotations from the container's OCI spec that hull
// takes on its command line, so the Linux path is a pass-through and the
// annotation names must match exactly.
func TestNerdctlPassesBootAnnotations(t *testing.T) {
	kernel, initrd := stageBootAssets(t)

	n := &nerdctl{bin: "/usr/local/bin/nerdctl"}
	args, _, err := n.runArgs(RunSpec{Name: "s", Image: "ubuntu:latest", GenericBoot: true})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	if !strings.Contains(got, "--annotation com.urunc.unikernel.bootKernel="+kernel) {
		t.Errorf("boot kernel annotation not passed: %s", got)
	}
	if !strings.Contains(got, "--annotation com.urunc.unikernel.bootInitrd="+initrd) {
		t.Errorf("boot initrd annotation not passed: %s", got)
	}
	if !strings.HasSuffix(got, "ubuntu:latest sleep infinity") {
		t.Errorf("image and parked command must stay last: %s", got)
	}
}

// docker does not carry annotations through to the runtime, so the sandbox
// would boot without a kernel. Refuse where the cause is still visible.
func TestNerdctlRefusesGenericBootOnDocker(t *testing.T) {
	stageBootAssets(t)

	d := &nerdctl{bin: "/usr/bin/docker"}
	_, _, err := d.runArgs(RunSpec{Name: "s", Image: "ubuntu:latest", GenericBoot: true})
	if err == nil {
		t.Fatal("expected genericBoot to be refused on docker")
	}
	if !strings.Contains(err.Error(), "nerdctl") {
		t.Errorf("error does not say what to use instead: %v", err)
	}

	// Without genericBoot docker is fine: nothing needs an annotation.
	if _, _, err := d.runArgs(RunSpec{Name: "s", Image: "ubuntu:latest"}); err != nil {
		t.Errorf("docker must still run an ordinary profile: %v", err)
	}
}

func TestNerdctlOmitsAnnotationsWithoutGenericBoot(t *testing.T) {
	n := &nerdctl{bin: "/usr/local/bin/nerdctl"}
	args, _, err := n.runArgs(RunSpec{Name: "s", Image: "img", RootfsType: "block"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	if strings.Contains(got, "--annotation") {
		t.Errorf("annotations must not appear for an ordinary profile: %s", got)
	}
	// RootfsType is a VM concept urunc decides on Linux; nerdctl has no flag
	// for it and must not invent one.
	if strings.Contains(got, "block") {
		t.Errorf("rootfs type leaked onto the nerdctl command line: %s", got)
	}
}

// A GUI profile has nowhere to draw on this path: the container runtime has no
// display on either driver. hull refuses the same profile when its backend
// cannot show a window; the container path must refuse it too, before any
// command runs, rather than boot headless and drop the window in silence. The
// stub would exit non-zero and say so if it ran, so the refusal must carry the
// reason and the alternative, not what the stub said.
func TestNerdctlRefusesGUIBeforeRunning(t *testing.T) {
	n := &nerdctl{bin: stubRuntimeBin(t, "STUB RAN", 1)}

	err := n.Run(RunSpec{Name: "brig-x", Image: "img", GUI: true})
	if err == nil {
		t.Fatal("expected a GUI profile to be refused on the container path")
	}
	if strings.Contains(err.Error(), "STUB RAN") {
		t.Errorf("a command was built and run before the refusal: %v", err)
	}
	// The message must name why this path cannot do it and where it can.
	if !strings.Contains(err.Error(), "container runtime") {
		t.Errorf("the refusal does not say why this path cannot show a window: %v", err)
	}
	if !strings.Contains(err.Error(), "macOS") {
		t.Errorf("the refusal does not point at where it can run: %v", err)
	}
}

// The refusal lives in CanRun, not just Run, and this pins the property that
// matters: a sandbox already up is joined rather than booted, so Run is never
// called on the second `brig run`. Only a check reachable through CanRun covers
// that join path, so assert CanRun itself refuses a GUI spec.
func TestNerdctlCanRunRefusesGUI(t *testing.T) {
	n := &nerdctl{bin: "/usr/local/bin/nerdctl"}

	err := n.CanRun(RunSpec{Name: "brig-x", Image: "img", GUI: true})
	if err == nil {
		t.Fatal("expected CanRun to refuse a GUI profile so the join path is covered")
	}
	if !strings.Contains(err.Error(), "container runtime") {
		t.Errorf("the refusal does not say why this path cannot show a window: %v", err)
	}
	if !strings.Contains(err.Error(), "macOS") {
		t.Errorf("the refusal does not point at where it can run: %v", err)
	}

	// A non-GUI profile passes CanRun, so the ordinary boot path is unaffected.
	if err := n.CanRun(RunSpec{Name: "brig-x", Image: "img"}); err != nil {
		t.Errorf("CanRun must let an ordinary profile through: %v", err)
	}
}

// A non-GUI profile is untouched by the refusal: it reaches the runtime and
// boots as any ordinary profile does.
func TestNerdctlRunsNonGUIProfile(t *testing.T) {
	n := &nerdctl{bin: stubRuntimeBin(t, "pulling ghcr.io/x", 0)}

	if err := n.Run(RunSpec{Name: "brig-x", Image: "img"}); err != nil {
		t.Fatalf("a non-GUI profile must still run: %v", err)
	}
}

// The guest architecture follows the host, and a Linux kernel is named for the
// architecture it boots.
func TestBootKernelNameFollowsArch(t *testing.T) {
	got := bootKernelName()
	if got != "Image" && got != "bzImage" {
		t.Fatalf("unexpected kernel name %q", got)
	}
}

// nerdctl's tmpfs is a real --tmpfs at create time; there is no privileged
// exec to mount one. The flag has to reach runArgs or a files: delivery
// writes onto the container's own writable layer.
func TestTmpfsReachesTheCreateLine(t *testing.T) {
	n := &nerdctl{bin: "nerdctl"}
	args, _, err := n.runArgs(RunSpec{Name: "x", Image: "i", Tmpfs: []string{"/run/brig/secrets:size=1m,mode=0700"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "--tmpfs /run/brig/secrets:size=1m,mode=0700") {
		t.Errorf("args = %v", args)
	}
}

// A resolved digest is what boots: the image on the command line is
// repo@sha256:..., not the tag, so containerd boots the object verify checked.
func TestNerdctlBootsTheVerifiedDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	n := &nerdctl{bin: "nerdctl"}
	args, _, err := n.runArgs(RunSpec{Name: "s", Image: "ghcr.io/brig-sh/claude-code:arm64", Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	if !strings.Contains(got, "ghcr.io/brig-sh/claude-code@"+digest+" sleep infinity") {
		t.Errorf("the tag was booted instead of the digest: %s", got)
	}
	if strings.Contains(got, ":arm64") {
		t.Errorf("the tag was left on the reference alongside the digest: %s", got)
	}
}

// With no digest resolved -- the hull path, or a run that could not reach the
// registry -- the tag boots as given, unchanged.
func TestNerdctlBootsTheTagWhenNoDigestResolved(t *testing.T) {
	n := &nerdctl{bin: "nerdctl"}
	args, _, err := n.runArgs(RunSpec{Name: "s", Image: "ghcr.io/brig-sh/claude-code:arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); !strings.HasSuffix(got, "ghcr.io/brig-sh/claude-code:arm64 sleep infinity") {
		t.Errorf("the tag was not booted as given: %s", got)
	}
}

func TestWithDigest(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	cases := map[string]string{
		"ghcr.io/brig-sh/x:arm64":     "ghcr.io/brig-sh/x@" + d,
		"ghcr.io/brig-sh/x":           "ghcr.io/brig-sh/x@" + d,
		"ghcr.io:443/brig-sh/x:arm64": "ghcr.io:443/brig-sh/x@" + d,
		"repo@sha256:old":             "repo@" + d,
	}
	for in, want := range cases {
		if got := withDigest(in, d); got != want {
			t.Errorf("withDigest(%q) = %q, want %q", in, got, want)
		}
	}
	// An empty digest leaves the reference untouched.
	if got := withDigest("ghcr.io/brig-sh/x:arm64", ""); got != "ghcr.io/brig-sh/x:arm64" {
		t.Errorf("withDigest with no digest = %q, want the tag unchanged", got)
	}
}

func TestRepoDigest(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	if got := repoDigest("ghcr.io/brig-sh/claude-code@" + d + "\n"); got != d {
		t.Errorf("repoDigest = %q, want the bare digest %q", got, d)
	}
	// A miss must read as no local copy, not as a digest.
	for _, in := range []string{"", "<no value>", "no-at-sign"} {
		if got := repoDigest(in); got != "" {
			t.Errorf("repoDigest(%q) = %q, want empty", in, got)
		}
	}
}

func TestBootArtifactsReportsWhichFileIsMissing(t *testing.T) {
	dir := t.TempDir()
	// Only the initrd: the kernel is the one that should be named.
	if err := os.WriteFile(filepath.Join(dir, bootInitrdName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_BOOT_ASSETS", dir)

	_, _, err := bootArtifacts(nil, nil)
	if err == nil {
		t.Fatal("expected a missing kernel to fail")
	}
	if !strings.Contains(err.Error(), bootKernelName()) {
		t.Errorf("error does not name the missing kernel: %v", err)
	}
}

// A sandbox asking for its own network gets one, by name. On Linux the default
// bridge is an ordinary layer 2 segment and two sandboxes on it reach each
// other, which is the gap isolated closes; on macOS the backends already keep
// guests apart, so this is the one runtime where the posture has work to do.
func TestNerdctlIsolatedAsksForItsOwnNetwork(t *testing.T) {
	n := &nerdctl{bin: "nerdctl"}
	args, _, err := n.runArgs(RunSpec{Name: "brig-x", Image: "img", Mem: 1, CPUs: 1, Net: "isolated"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	if !strings.Contains(got, "--network "+sandboxNetwork("brig-x")) {
		t.Errorf("isolated did not ask for its own network: %s", got)
	}
	// Shared stays as it was: no --network at all, so the runtime's default
	// bridge is used and nothing about existing sandboxes changes.
	args, _, err = n.runArgs(RunSpec{Name: "brig-x", Image: "img", Mem: 1, CPUs: 1, Net: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if got = strings.Join(args, " "); strings.Contains(got, "--network") {
		t.Errorf("shared asked for a network explicitly: %s", got)
	}
	// Offline is unchanged too.
	args, _, err = n.runArgs(RunSpec{Name: "brig-x", Image: "img", Mem: 1, CPUs: 1, Net: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if got = strings.Join(args, " "); !strings.Contains(got, "--network none") {
		t.Errorf("offline did not ask for no network: %s", got)
	}
}

// The network is named after the sandbox, so a leaked one is traceable to what
// left it and `brig reset` can find every one brig made.
func TestSandboxNetworkIsNamedAfterTheSandbox(t *testing.T) {
	if got := sandboxNetwork("brig-claude-code-foo"); got != "brig-claude-code-foo" {
		t.Errorf("network name = %q", got)
	}
	if !strings.HasPrefix(sandboxNetwork("brig-x"), "brig-") {
		t.Error("a brig network is not identifiable as brig's")
	}
}

// A network whose sandbox was removed outside brig is not reachable through
// Remove, because the sandbox is not in the list any more. reset is the verb
// whose whole job is leaving nothing behind, so it prunes those -- and must
// leave alone both the networks still in use and every network brig did not
// make.
func TestPruneNetworksKeepsWhatIsInUseAndWhatIsNotOurs(t *testing.T) {
	all := []string{"bridge", "host", "none", "brig-claude-code", "brig-claude-code-gone", "my-own-net"}
	inUse := []string{"brig-claude-code"}

	got := prunableNetworks(all, inUse)

	want := map[string]bool{"brig-claude-code-gone": true}
	if len(got) != len(want) {
		t.Fatalf("prunable = %v, want just the leaked brig network", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("would have removed %q, which is either in use or not brig's", n)
		}
	}
}
