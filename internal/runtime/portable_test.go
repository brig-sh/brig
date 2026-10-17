package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// asArch makes this host the architecture a test asks about, for the
// decisions that follow it.
func asArch(t *testing.T, arch string) {
	t.Helper()
	was := guestArch
	guestArch = arch
	t.Cleanup(func() { guestArch = was })
}

// A teleportable session boots the portable machine on arm64 and hvi's
// native one anywhere else, where there is no portable machine.
func TestTeleportableMachineFollowsTheGuestArch(t *testing.T) {
	for arch, want := range map[string]string{"arm64": MachinePortableArm64V1, "amd64": ""} {
		asArch(t, arch)
		if got := TeleportableMachine(); got != want {
			t.Errorf("on %s a teleportable session boots %q, want %q", arch, got, want)
		}
	}
}

// The pin is the five-patch Image that hvi's tools/guest/kernel/README.md
// names, not an earlier one. That README lists three: the two-patch Image
// 32725b9b counts one move two or more times on a guest with more than one
// vCPU, the three-patch Image 4b2e8c5c counts a host sleep as a move, and
// the four-patch Image e85e4338 tells user space nothing of a move and
// cannot destroy a socket, so an API stream the guest only reads from
// stays open about a minute after it. Putting any of them back would boot
// it on every teleportable session with no other test failing, since the
// tests that compare a kernel with the pin swap in a digest of their own.
func TestPortableKernelDigestPinsTheFivePatchImage(t *testing.T) {
	const want = "sha256:778700360da1641466b5b9791a0fe231a52a20c4fb221e1b0e2cdf42730ed295"
	if PortableKernelDigest != want {
		t.Errorf("PortableKernelDigest = %s, want the five-patch Image %s", PortableKernelDigest, want)
	}
	hex, ok := strings.CutPrefix(PortableKernelDigest, "sha256:")
	if !ok || len(hex) != 64 || strings.Trim(hex, "0123456789abcdef") != "" {
		t.Errorf("PortableKernelDigest %q is not sha256: and 64 lowercase hex digits", PortableKernelDigest)
	}
}

// None of the Images before the current one may come back as the pin. The
// name is from when the four-patch Image e85e4338 was the pin and stays
// because check-tests-kept tracks test names; the five-patch test above is
// the one that pins the current Image.
func TestPortableKernelDigestPinsTheFourPatchImage(t *testing.T) {
	for _, old := range []string{
		"sha256:e85e4338feeb361c5329b4105fa3bda5cd72a235c9ae16c3c4ff94728b12aa1f",
		"sha256:4b2e8c5ce382a2635efe1b1b198e35b081d2f0cd4d39d9689b026fbc7d8f21b7",
		"sha256:32725b9b4327a8833d833e2a017f510310504309a8ed1d1030ef0118d018a152",
	} {
		if PortableKernelDigest == old {
			t.Errorf("PortableKernelDigest = %s, an Image from before the fifth patch", old)
		}
	}
}

// writeFile writes a non-empty file at path.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The portable machine's run line is the native one with two changes and no
// others: --profile portable-arm64-v1 after the root type, and the portable
// kernel in the kernel annotation. The initrd is the bundle's on both.
func TestRunArgsForThePortableMachineChangeOnlyTheProfileAndTheKernel(t *testing.T) {
	native := RunSpec{Name: "brig-s", Image: "ubuntu:latest", Mem: 1024, CPUs: 1, RootfsType: "block",
		GenericBoot: true, Teleportable: true,
		BootAssets: BootAssets{Kernel: "/a/Image", Initrd: "/a/container-initrd"}}
	portable := native
	portable.Profile = MachinePortableArm64V1
	portable.BootAssets = BootAssets{Kernel: "/a/" + portableKernelPath, Initrd: "/a/container-initrd",
		Machine: MachinePortableArm64V1}

	before, _, err := runArgs(native, "hvi", "none", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := runArgs(portable, "hvi", "none", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(before, "--profile") {
		t.Errorf("the native machine's run line names a profile: %v", before)
	}
	at := slices.Index(after, "--profile")
	if at < 0 || at+1 >= len(after) || after[at+1] != MachinePortableArm64V1 {
		t.Fatalf("the portable run line does not ask for %s: %v", MachinePortableArm64V1, after)
	}
	if after[at-2] != "--rootfs-type" {
		t.Errorf("--profile is not right after the root type: %v", after)
	}
	rest := slices.Delete(slices.Clone(after), at, at+2)
	kernel := annotationBootKernel + "=/a/" + portableKernelPath
	i := slices.Index(rest, kernel)
	if i < 0 {
		t.Fatalf("the portable kernel is not the one that boots: %v", after)
	}
	rest[i] = annotationBootKernel + "=/a/Image"
	if !slices.Equal(rest, before) {
		t.Errorf("the portable run line changes more than the profile and the kernel:\nnative   %v\nportable %v",
			before, after)
	}
}

// Paths resolved for one machine never boot another: a teleportable boot
// handed the bundle's kernel is refused rather than booting a guest that
// could not move.
func TestRunArgsRefusesBootAssetsResolvedForAnotherMachine(t *testing.T) {
	spec := RunSpec{Name: "s", Image: "img", GenericBoot: true, Profile: MachinePortableArm64V1,
		BootAssets: BootAssets{Kernel: "/a/Image", Initrd: "/a/container-initrd"}}
	if _, _, err := runArgs(spec, "hvi", "none", "", "", nil, nil); err == nil {
		t.Fatal("a portable boot took the native machine's kernel")
	}
	spec.Profile = ""
	spec.BootAssets.Machine = MachinePortableArm64V1
	if _, _, err := runArgs(spec, "hvi", "none", "", "", nil, nil); err == nil {
		t.Fatal("a native boot took the portable kernel")
	}
}

// A run that resolves for itself swaps in the portable kernel too, so a
// runtime asked without a resolve ahead of it boots the same files.
func TestRunArgsResolvingForThePortableMachineBootsThePortableKernel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_BOOT_ASSETS", dir)
	t.Setenv("BRIG_PORTABLE_KERNEL", "")
	writeFile(t, filepath.Join(dir, bootKernelName()), "bundle kernel")
	writeFile(t, filepath.Join(dir, bootInitrdName), "bundle initrd")
	writeFile(t, filepath.Join(dir, portableKernelPath), "portable kernel")

	got := argv(t, RunSpec{Name: "s", Image: "img", GenericBoot: true, Profile: MachinePortableArm64V1},
		"hvi", "none", "", "")

	if !strings.Contains(got, annotationBootKernel+"="+filepath.Join(dir, portableKernelPath)) ||
		!strings.Contains(got, annotationBootInitrd+"="+filepath.Join(dir, bootInitrdName)) {
		t.Errorf("the portable boot does not take the portable kernel and the bundle's initrd: %s", got)
	}
}

// BRIG_PORTABLE_KERNEL names the kernel and wins. Unset, the kernel is a
// file of its own in the boot asset directory. A missing one refuses and
// names the variable; nothing fetches it.
func TestResolvingThePortableKernel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_BOOT_ASSETS", "")
	t.Setenv("BRIG_PORTABLE_KERNEL", "")
	writeFile(t, filepath.Join(dir, bootKernelName()), "bundle kernel")
	writeFile(t, filepath.Join(dir, bootInitrdName), "bundle initrd")
	locate := func() (string, error) { return dir, nil }
	fetch := func(string) error {
		t.Error("a resolve fetched, with the bundle in place")
		return errors.New("test: no fetch")
	}

	_, err := resolveMachineAssets(locate, fetch, BootFetch{Machine: MachinePortableArm64V1})
	var missing *PortableKernelMissingError
	if !errors.As(err, &missing) || missing.Named || !strings.Contains(err.Error(), "BRIG_PORTABLE_KERNEL") ||
		!strings.Contains(err.Error(), filepath.Join(dir, portableKernelPath)) {
		t.Fatalf("a missing portable kernel resolved, or said nothing of where it goes: %v", err)
	}

	writeFile(t, filepath.Join(dir, portableKernelPath), "portable kernel")
	got, err := resolveMachineAssets(locate, fetch, BootFetch{Machine: MachinePortableArm64V1})
	if err != nil {
		t.Fatal(err)
	}
	want := BootAssets{Kernel: filepath.Join(dir, portableKernelPath), Initrd: filepath.Join(dir, bootInitrdName),
		Machine: MachinePortableArm64V1}
	if got != want {
		t.Errorf("the default place resolved %+v, want %+v", got, want)
	}

	named := filepath.Join(t.TempDir(), "my-kernel")
	writeFile(t, named, "my portable kernel")
	t.Setenv("BRIG_PORTABLE_KERNEL", named)
	got, err = resolveMachineAssets(locate, fetch, BootFetch{Machine: MachinePortableArm64V1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kernel != named || !got.KernelNamed || got.Initrd != want.Initrd {
		t.Errorf("BRIG_PORTABLE_KERNEL resolved %+v, want its kernel, named, beside the bundle's initrd", got)
	}

	// The native machine keeps the bundle's kernel, whatever is set.
	got, err = resolveMachineAssets(locate, fetch, BootFetch{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kernel != filepath.Join(dir, bootKernelName()) || got.Machine != "" {
		t.Errorf("the native machine resolved %+v, want the bundle's kernel", got)
	}

	if _, err := resolveMachineAssets(locate, fetch, BootFetch{Machine: "portable-x86-v9"}); err == nil {
		t.Error("a machine brig does not know resolved")
	}
}

// An empty file is a missing kernel, as it is for the bundle's.
func TestAnEmptyPortableKernelIsMissing(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "Image")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_PORTABLE_KERNEL", empty)
	_, err := forMachine(BootAssets{Kernel: "/a/Image", Initrd: "/a/i"}, MachinePortableArm64V1, nil)
	var missing *PortableKernelMissingError
	if !errors.As(err, &missing) || !missing.Named {
		t.Fatalf("an empty named kernel resolved: %v", err)
	}
}

// Only hvi boots a machine profile. nerdctl refuses one rather than booting
// a machine other than the one asked for.
func TestNerdctlRefusesAMachineProfile(t *testing.T) {
	n := &nerdctl{bin: "nerdctl"}
	if _, _, err := n.runArgs(RunSpec{Name: "s", Image: "img", Profile: MachinePortableArm64V1}); err == nil {
		t.Error("nerdctl took a machine profile")
	}
	if _, err := n.ResolveBootAssets(BootFetch{Machine: MachinePortableArm64V1}, nil, nil); err == nil {
		t.Error("nerdctl resolved boot assets for a machine profile")
	}
}

// hull's record of the profile wins, then what hvi's own profile file names. Without it, a boot's argv decides:
// its --profile, or the native machine when it names none. A restore's argv
// does not say, and neither does a VMM that is not hvi.
func TestMachineOfReadsWhatHviRuns(t *testing.T) {
	profiles := map[string]string{
		"/i/portable.json": `{"profile_version":1,"profile":"portable-arm64-v1"}`,
		"/i/older.json":    `{"profile_version":1}`,
	}
	read := func(path string) ([]byte, error) {
		if p, ok := profiles[path]; ok {
			return []byte(p), nil
		}
		return nil, os.ErrNotExist
	}
	cases := []struct{ record, want string }{
		{`{"cmdLine":["hvi","restore","--from","/b"],"profile":"portable-arm64-v1"}`, MachinePortableArm64V1},
		{`{"cmdLine":["hvi","restore","--from","/b"],"profilePath":"/i/portable.json"}`, MachinePortableArm64V1},
		{`{"cmdLine":["/x/hvi","boot","--disk","/d","--profile","portable-arm64-v1"],"profilePath":"/i/older.json"}`, MachinePortableArm64V1},
		{`{"cmdLine":["hvi","boot","--disk","/d"],"profilePath":"/i/missing.json"}`, MachineNative},
		{`{"cmdLine":["hvi","boot","--disk","/d"]}`, MachineNative},
		{`{"cmdLine":["hvi","restore","--from","/b"]}`, ""},
		{`{"cmdLine":["vz-helper","--disk","/d"]}`, ""},
		{`{"name":"s"}`, ""},
	}
	for _, tc := range cases {
		got, err := machineOf([]byte(tc.record), read)
		if err != nil {
			t.Errorf("%s: %v", tc.record, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s reads as %q, want %q", tc.record, got, tc.want)
		}
	}
	if _, err := machineOf([]byte("not json"), read); err == nil {
		t.Error("a record that is not JSON read as a machine")
	}
}

// A run that names a machine and has no boot assets would leave hull to pick
// the kernel. runArgs refuses it, so no caller can build that line.
func TestRunArgsRefusesAMachineWithNoBootAssets(t *testing.T) {
	spec := RunSpec{Name: "s", Image: "img", Profile: MachinePortableArm64V1}
	if _, _, err := runArgs(spec, "hvi", "none", "", "", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "genericBoot") {
		t.Fatalf("a machine with no boot assets got a run line: %v", err)
	}
}

// What `hull run --help` prints around --profile, cut from a hull with
// machine profiles and from v0.1.0-rc29, which has none.
const (
	hullHelpWithProfile = "OPTIONS:\n" +
		"   --profile hull assets pull --profile   hvi machine profile: 'native' (default) or 'portable-arm64-v1'\n" +
		"   --rootfs-type string                   rootfs sharing mode\n"
	hullHelpWithoutProfile = "OPTIONS:\n" +
		"   --qemu-path string                     path to qemu-system-aarch64 binary\n" +
		"   --rootfs-type string                   rootfs sharing mode\n" +
		"   --profiles-dir string                  a flag that only starts with the name\n"
)

// brig asks hull for the machine with --profile, which an older hull does
// not take. The probe tells the two apart from the help text, refuses the
// older one as unable to run a teleportable session, and refuses a probe
// that failed as unknown.
func TestHullTakesProfileReadsRunHelp(t *testing.T) {
	current := &hull{bin: scriptHull(t, "[ \"$1 $2\" = 'run --help' ] || exit 9\ncat <<'EOF'\n"+hullHelpWithProfile+"EOF\n")}
	if err := current.takesProfile("hvi", MachinePortableArm64V1); err != nil {
		t.Errorf("a hull that lists --profile was refused: %v", err)
	}

	old := &hull{bin: scriptHull(t, "case \"$1\" in --version) echo 'hull v0.1.0-rc29 (e54923f)';; "+
		"run) cat <<'EOF'\n"+hullHelpWithoutProfile+"EOF\n;; esac\n")}
	err := old.takesProfile("hvi", MachinePortableArm64V1)
	var ce *CapabilityError
	if !errors.As(err, &ce) || ce.Property != Teleportable || ce.State != CannotEnforce {
		t.Fatalf("a hull with no --profile: want a teleportable CapabilityError it cannot enforce, got %v", err)
	}
	for _, want := range []string{"0.1.0-rc29", "--profile", MachinePortableArm64V1, "Upgrade hull"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}

	missing := &hull{bin: filepath.Join(t.TempDir(), "not-a-hull")}
	err = missing.takesProfile("hvi", MachinePortableArm64V1)
	if !errors.As(err, &ce) || ce.State != Unknown || !strings.Contains(err.Error(), "run --help") {
		t.Fatalf("a probe that could not run: want an unknown CapabilityError naming the probe, got %v", err)
	}
}

// A run that names a machine on a hull without --profile stops at the probe:
// no gateway, no run. A run that names none never pays for the probe.
func TestARunNamingAMachineStopsOnAHullWithoutTheFlag(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls")
	bin := scriptHull(t, "echo \"$*\" >> '"+log+"'\n"+
		"[ \"$1 $2\" = 'run --help' ] && { cat <<'EOF'\n"+hullHelpWithoutProfile+"EOF\nexit 0; }\nexit 3\n")
	h := &hull{bin: bin}
	spec := RunSpec{Name: "s", Image: "img", Hypervisor: "hvi", Net: "none", GenericBoot: true,
		Profile:    MachinePortableArm64V1,
		BootAssets: BootAssets{Kernel: "/a/Image", Initrd: "/a/initrd", Machine: MachinePortableArm64V1}}

	err := h.Run(spec)

	var ce *CapabilityError
	if !errors.As(err, &ce) || ce.Property != Teleportable {
		t.Fatalf("want a teleportable CapabilityError, got %v", err)
	}
	calls, _ := os.ReadFile(log)
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if strings.HasPrefix(line, "run ") && line != "run --help" {
			t.Errorf("hull was asked to run after the probe refused: %q", line)
		}
	}

	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	spec.Profile, spec.BootAssets.Machine = "", ""
	_ = h.Run(spec)
	calls, _ = os.ReadFile(log)
	if strings.Contains(string(calls), "run --help") {
		t.Errorf("a run that names no machine probed hull: %s", calls)
	}
}
