package runtime

import (
	"strings"
	"testing"
)

// argv is the flags as one string, so a test can ask whether a flag and its
// value were emitted together rather than hunting for an index.
func argv(t *testing.T, spec RunSpec, hv, net, gatewaySock, gatewayCidr string) string {
	t.Helper()
	args, _, err := runArgs(spec, hv, net, gatewaySock, gatewayCidr, nil, nil)
	if err != nil {
		t.Fatalf("runArgs: %v", err)
	}
	return strings.Join(args, " ")
}

func TestRunArgsDefaultsStayOffTheCommandLine(t *testing.T) {
	got := argv(t, RunSpec{Name: "s", Image: "img", Mem: 2048, CPUs: 2}, "vz", "shared", "", "")

	// An unset rootfs type must not become a flag: hull's own default differs
	// per backend, and sending an empty value would override it.
	for _, flag := range []string{"--rootfs-type", "--annotation", "--gateway-sock", "--gui"} {
		if strings.Contains(got, flag) {
			t.Errorf("unset option reached the command line as %s: %s", flag, got)
		}
	}
	if !strings.Contains(got, "--hypervisor vz") {
		t.Errorf("hypervisor not passed: %s", got)
	}
	if !strings.HasSuffix(got, " img") {
		t.Errorf("image must be the final argument: %s", got)
	}
}

func TestRunArgsRootfsTypeAndGateway(t *testing.T) {
	got := argv(t, RunSpec{Name: "s", Image: "img", RootfsType: "block"}, "hvi", "shared", "/tmp/gw.sock", "198.18.0.2/24")
	if !strings.Contains(got, "--rootfs-type block") {
		t.Errorf("rootfs type not passed: %s", got)
	}
	// hull refuses one of these without the other, so they must always be
	// emitted as a pair. Passing only the socket is what made the first real
	// hvi boot fail with "--gateway-sock and --gateway-cidr must be used
	// together".
	if !strings.Contains(got, "--gateway-sock /tmp/gw.sock") {
		t.Errorf("gateway socket not passed: %s", got)
	}
	if !strings.Contains(got, "--gateway-cidr 198.18.0.2/24") {
		t.Errorf("gateway CIDR not passed alongside the socket: %s", got)
	}
}

// The pairing is the invariant, not the individual flags: hull rejects either
// one alone, so neither may reach the command line without the other.
func TestRunArgsGatewayFlagsTravelTogether(t *testing.T) {
	for _, tt := range []struct {
		name, sock, cidr string
	}{
		{name: "neither"},
		{name: "both", sock: "/tmp/gw.sock", cidr: "198.18.0.7/24"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := argv(t, RunSpec{Name: "s", Image: "img"}, "hvi", "shared", tt.sock, tt.cidr)
			hasSock := strings.Contains(got, "--gateway-sock")
			hasCidr := strings.Contains(got, "--gateway-cidr")
			if hasSock != hasCidr {
				t.Fatalf("gateway flags must appear together, got sock=%t cidr=%t: %s",
					hasSock, hasCidr, got)
			}
		})
	}
}

// hull takes the layout of the root from HULL_ROOTFS_MODE, so the mode goes in
// its environment and nowhere on its command line. A generic boot gets it by
// default where hull's mke2fs is there, and not where it is missing.
func TestRunArgsRootfsModeGoesToHullEnv(t *testing.T) {
	stageBootAssets(t)
	hullMode := func(spec RunSpec, hv string) string {
		t.Helper()
		args, env, err := runArgs(spec, hv, "shared", "", "", nil, nil)
		if err != nil {
			t.Fatalf("%+v on %s: %v", spec, hv, err)
		}
		if got := strings.Join(args, " "); strings.Contains(got, "overlay-block") {
			t.Errorf("the mode leaked onto hull's command line: %s", got)
		}
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "HULL_ROOTFS_MODE="); ok {
				return v
			}
		}
		return ""
	}
	generic := RunSpec{Name: "s", Image: "ubuntu:latest", GenericBoot: true}

	if got := hullMode(generic, "hvi"); got != "" {
		t.Errorf("no mke2fs for hull: HULL_ROOTFS_MODE=%s, want none", got)
	}
	explicit := generic
	explicit.RootfsMode = RootfsModeOverlayBlock
	if _, _, err := runArgs(explicit, "hvi", "shared", "", "", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "brew install e2fsprogs") {
		t.Errorf("BRIG_ROOTFS_MODE=overlay-block with no mke2fs: want a refusal naming e2fsprogs, got %v", err)
	}

	overlayHost(t)
	for _, hv := range []string{"vz", "hvi"} {
		if got := hullMode(generic, hv); got != RootfsModeOverlayBlock {
			t.Errorf("%s: HULL_ROOTFS_MODE=%q by default, want overlay-block", hv, got)
		}
		if got := hullMode(explicit, hv); got != RootfsModeOverlayBlock {
			t.Errorf("%s: HULL_ROOTFS_MODE=%q when asked, want overlay-block", hv, got)
		}
	}
	optOut := generic
	optOut.RootfsMode = RootfsModeVirtiofs
	block := generic
	block.RootfsType = "block"
	for _, spec := range []RunSpec{optOut, block, {Name: "s", Image: "img"}} {
		if got := hullMode(spec, "hvi"); got != "" {
			t.Errorf("%+v: HULL_ROOTFS_MODE=%s, want none", spec, got)
		}
	}
	if got := hullMode(generic, "qemu"); got != "" {
		t.Errorf("qemu: HULL_ROOTFS_MODE=%s by default, want none", got)
	}
	for _, bad := range []RunSpec{
		{Name: "s", Image: "img", RootfsMode: RootfsModeOverlayBlock},
		{Name: "s", Image: "ubuntu:latest", GenericBoot: true, RootfsMode: "block"},
		{Name: "s", Image: "ubuntu:latest", GenericBoot: true, RootfsType: "block", RootfsMode: RootfsModeOverlayBlock},
	} {
		if _, _, err := runArgs(bad, "hvi", "shared", "", "", nil, nil); err == nil {
			t.Errorf("%+v: want a refusal", bad)
		}
		if err := supports(bad, "hvi"); err == nil {
			t.Errorf("%+v: CanRun lets it through", bad)
		}
	}
}

// The row names the root hull boots: overlay-block where brig hands hull that
// mode, and the rootfs type or hull's default where it hands none.
func TestHullNamesTheRootfsModeItPasses(t *testing.T) {
	h := &hull{bin: "hull"}
	generic := RunSpec{GenericBoot: true, Hypervisor: "hvi"}
	if got := h.Isolation(generic).Line(); !strings.HasSuffix(got, "rootfs hull default)") {
		t.Errorf("no mke2fs for hull: %s", got)
	}
	overlayHost(t)
	if got := h.Isolation(generic).Line(); !strings.HasSuffix(got, "hvi backend, rootfs overlay-block)") {
		t.Errorf("the row does not name the overlay root brig passes: %s", got)
	}
	optOut := generic
	optOut.RootfsMode = RootfsModeVirtiofs
	optOut.RootfsType = "virtiofs"
	if got := h.Isolation(optOut).Line(); !strings.HasSuffix(got, "rootfs virtiofs)") {
		t.Errorf("BRIG_ROOTFS_MODE=virtiofs: %s", got)
	}
	if got := h.Isolation(RunSpec{Hypervisor: "hvi"}).Line(); strings.Contains(got, "overlay") {
		t.Errorf("a boot that brings its own root names an overlay: %s", got)
	}
}

func TestRunArgsGenericBootPassesHostArtifacts(t *testing.T) {
	kernel, initrd := stageBootAssets(t)

	got := argv(t, RunSpec{Name: "s", Image: "ubuntu:latest", GenericBoot: true}, "hvi", "shared", "", "")
	if !strings.Contains(got, "--annotation com.urunc.unikernel.bootKernel="+kernel) {
		t.Errorf("boot kernel not passed: %s", got)
	}
	if !strings.Contains(got, "--annotation com.urunc.unikernel.bootInitrd="+initrd) {
		t.Errorf("boot initrd not passed: %s", got)
	}
}

// A missing artifact must be reported before boot and must name the path, so
// the user learns which file to supply rather than watching a VM fail to come
// up.
func TestRunArgsGenericBootReportsMissingArtifact(t *testing.T) {
	t.Setenv("BRIG_BOOT_ASSETS", t.TempDir())

	_, _, err := runArgs(RunSpec{Name: "s", Image: "ubuntu:latest", GenericBoot: true}, "hvi", "shared", "", "", nil, nil)
	if err == nil {
		t.Fatal("expected a missing boot artifact to fail")
	}
	if !strings.Contains(err.Error(), bootKernelName()) {
		t.Errorf("error does not name the missing file: %v", err)
	}
}

// Values never travel in argv, where any process on the host could read them
// out of ps; only the bare name does.
func TestRunArgsKeepsSecretValuesOutOfArgv(t *testing.T) {
	spec := RunSpec{Name: "s", Image: "img", Env: []Var{{Name: "GH_TOKEN", Value: "sekrit"}}}
	args, env, err := runArgs(spec, "vz", "shared", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "sekrit") {
		t.Fatalf("secret value reached argv: %s", joined)
	}
	if !strings.Contains(joined, "--env GH_TOKEN") {
		t.Errorf("variable name not passed: %s", joined)
	}
	if len(env) != 1 || env[0] != "GH_TOKEN=sekrit" {
		t.Errorf("value did not travel in the environment: %v", env)
	}
}

func TestRunArgsReadOnlyShareCarriesMode(t *testing.T) {
	spec := RunSpec{Name: "s", Image: "img", Shares: []Share{
		{Host: "/w", Guest: "/root/work"},
		{Host: "/skills", Guest: "/root/.claude/skills", ReadOnly: true},
	}}
	got := argv(t, spec, "hvi", "shared", "", "")
	if !strings.Contains(got, "--shared-dir /w:/root/work ") {
		t.Errorf("writable share must carry no mode suffix: %s", got)
	}
	if !strings.Contains(got, "--shared-dir /skills:/root/.claude/skills:ro") {
		t.Errorf("read-only share lost its mode: %s", got)
	}
}

// A graphical profile on a backend without a console is refused here, not at
// boot, and the message names the variable that caused it.
func TestSupportsRefusesGUIOffVz(t *testing.T) {
	if err := supports(RunSpec{GUI: true}, "vz"); err != nil {
		t.Fatalf("vz must accept a GUI profile: %v", err)
	}
	err := supports(RunSpec{GUI: true}, "hvi")
	if err == nil {
		t.Fatal("expected a GUI profile to be refused on hvi")
	}
	if !strings.Contains(err.Error(), "BRIG_HYPERVISOR") {
		t.Errorf("error does not name the variable to unset: %v", err)
	}
	if err := supports(RunSpec{}, "hvi"); err != nil {
		t.Errorf("a non-GUI profile must run on hvi: %v", err)
	}
}

// The value goes on stdin and never in argv, because the guest agent logs
// every exec's argv, and on some backends that log is a host file that
// outlives the sandbox. This is the same rule
// Var.Secret already enforces for the environment channel, applied to the
// channel that is about to carry the same credentials.
func TestFeedKeepsTheValueOutOfArgv(t *testing.T) {
	h := &hull{bin: "hull"}
	args, _, err := h.execArgs(ExecSpec{
		Name:  "brig-claude-code",
		User:  "root",
		Cmd:   []string{"sh", "-c", "cat > /run/brig/secrets/claude-credentials"},
		Stdin: strings.NewReader("super-secret-value"),
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "super-secret-value") {
		t.Fatalf("the value reached argv: %s", joined)
	}
	if !strings.Contains(joined, "-u root") {
		t.Errorf("privileged exec lost its user: %s", joined)
	}
	// hull parses `sh -c ...` as its own flags without this.
	i := indexOf(args, "--")
	if i < 0 || args[i-1] != "brig-claude-code" {
		t.Errorf("-- does not separate the guest command: %s", joined)
	}
}

// indexOf is the position of s in args, or -1. A small local helper rather
// than slices.Index so the test reads without a second import just for this.
func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}

func TestQemuGatewaySocketMatchesHullConvention(t *testing.T) {
	if got := qemuGatewaySocket("/a/gw.sock"); got != "/a/gw.sock.qemu" {
		t.Errorf("qemuGatewaySocket = %q, want /a/gw.sock.qemu", got)
	}
}

func TestGatewaySocketHonoursOverride(t *testing.T) {
	t.Setenv("BRIG_GATEWAY_SOCK", "/custom/gw.sock")
	got, err := gatewaySocket()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/custom/gw.sock" {
		t.Errorf("gatewaySocket = %q, want the override", got)
	}
}

// hull's CanRun is supports, and a sandbox that is already up is joined, not
// booted. A rootfs mode checked only in runArgs would pass the join path.
func TestHullCanRunRefusesAnUnknownRootfsMode(t *testing.T) {
	overlayHost(t)
	h := &hull{}
	spec := RunSpec{Name: "s", Image: "ubuntu:latest", GenericBoot: true, RootfsMode: "overlay-blok"}
	if err := h.CanRun(spec); err == nil || !strings.Contains(err.Error(), "overlay-blok") {
		t.Errorf("CanRun must refuse an unknown rootfs mode by name: %v", err)
	}
	spec.RootfsMode = RootfsModeOverlayBlock
	if err := h.CanRun(spec); err != nil {
		t.Errorf("CanRun refused %s: %v", RootfsModeOverlayBlock, err)
	}
	spec.GenericBoot = false
	if err := h.CanRun(spec); err == nil {
		t.Error("CanRun must refuse the mode on a profile that is not a generic boot")
	}
}
