package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// backend is one place docs/policies.md says a policy is enforced. The cases
// and the verdict rules are the same on each: hull's gateway and brig's own
// nftables table on Linux refuse in the same modes, which the hand run in
// docs/manual-tests/egress-policy-linux.md measured. What differs is what
// boots, and so how a record names it.
type backend struct {
	// runtime is the binary brig drives there, when neither -runtime nor
	// BRIG_RUNTIME_BIN names one.
	runtime string
	// hypervisor goes into the profile and into BRIG_HYPERVISOR. It is empty
	// on Linux, where the containerd shim decides and brig ignores both.
	hypervisor string
}

var backends = map[string]backend{
	"hvi":     {runtime: "hull", hypervisor: "hvi"},
	"nerdctl": {runtime: "nerdctl"},
}

// backendNames lists the backends this runner boots, for a refusal to name.
func backendNames() string {
	var names []string
	for name := range backends {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// stackRow is one line of the record's table about what booted, beside the
// runtime and brig rows every backend has.
type stackRow struct{ name, value string }

// pins reads the Linux runtime bundle's pins.env through `brig-ctl version`,
// which prints it. The bundle is what install.sh installs and pins on Linux,
// so its version is what a record there is filed under, the way hull's is on
// macOS. A nerdctl the bundle did not install has none, and the error says so.
func (s *suite) pins(ctx context.Context, env []string) (map[string]string, error) {
	cctx, cancel := context.WithTimeout(ctx, rmTimeout)
	defer cancel()
	out, stderr, code, err := s.exec(cctx, env, s.brigCtl, "version")
	if err != nil || code != 0 {
		return nil, fmt.Errorf("%s version: %s", s.brigCtl, failure(stderr, code, err))
	}
	pins := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		pins[k] = strings.Trim(v, `"`)
	}
	if pins["BUNDLE_VERSION"] == "" {
		return nil, fmt.Errorf("%s version names no BUNDLE_VERSION", s.brigCtl)
	}
	return pins, nil
}

// bundleMismatch says why the runtime that reports runtimeLine is not the one
// the bundle in pins ships, or "" when it is. The bundle pins NERDCTL_VERSION
// beside every other part it carries.
func (s *suite) bundleMismatch(runtimeLine string, pins map[string]string) string {
	pinned := strings.TrimPrefix(pins["NERDCTL_VERSION"], "v")
	if pinned == "" {
		return fmt.Sprintf("the bundle %s names pins no NERDCTL_VERSION, so the runtime that boots cannot be tied to it", s.brigCtl)
	}
	if got := strings.TrimPrefix(versionOf(runtimeLine), "v"); got != pinned {
		return fmt.Sprintf("the runtime that boots reports nerdctl %q and the bundle %s names pins nerdctl %q: that bundle is not the one under test", got, s.brigCtl, pinned)
	}
	// The record's name carries the variant, and only ROOTLESS=true gave it
	// one: a missing key, or any other value, filed the run under the plain
	// bundle's name. Both pins the bundle writes are true or false, so
	// anything else is a pins.env this runner does not know how to read.
	switch pins["ROOTLESS"] {
	case "true", "false":
	default:
		return fmt.Sprintf("the bundle %s names pins ROOTLESS=%q, neither true nor false, so which bundle it is cannot be told", s.brigCtl, pins["ROOTLESS"])
	}
	return ""
}

// nerdctlStack is what the record says booted on Linux: the bundle and the
// parts of it a boot goes through, from its pins, and the nft that enforces
// the policy, which is the host's. Without pins it says why, and the record
// is filed under the nerdctl version instead.
func (s *suite) nerdctlStack(ctx context.Context, env []string, rep *report) []stackRow {
	pins, err := s.pins(ctx, env)
	if err != nil {
		return []stackRow{{"runtime bundle", "none: " + err.Error()}, s.nftRow(ctx, env)}
	}
	// The pins are the bundle brig-ctl names, and they say nothing of which
	// nerdctl booted. A record filed under a bundle that did not boot would
	// pass for the evidence docs/releasing.md gates a release on, so the
	// nerdctl that reports its version here has to be the one the bundle
	// pins. Without the pin there is nothing to tie it to, and that is no
	// reason to trust it.
	if why := s.bundleMismatch(rep.runtime, pins); why != "" {
		rep.problems = append(rep.problems, why)
		rep.unfiled = why
		return []stackRow{{"runtime bundle", "not used: " + why}, s.nftRow(ctx, env)}
	}
	rep.bundle = pins["BUNDLE_VERSION"]
	rep.pins = pins
	bundle := pins["BUNDLE_VERSION"]
	if v := pins["VARIANT"]; v != "" {
		bundle += ", " + v
	}
	if pins["ROOTLESS"] == "true" {
		bundle += ", rootless"
	}
	rows := []stackRow{{"runtime bundle", bundle}}
	for _, r := range []struct{ name, key string }{
		{"containerd", "CONTAINERD_VERSION"},
		{"urunc", "URUNC_VERSION"},
		{"monitors", "MONITORS_VERSION"},
		{"rootlesskit", "ROOTLESSKIT_VERSION"},
		{"slirp4netns", "SLIRP4NETNS_VERSION"},
	} {
		if v := pins[r.key]; v != "" {
			rows = append(rows, stackRow{r.name, v})
		}
	}
	return append(rows, s.nftRow(ctx, env))
}

// nftRow names the nft brig enforces a policy with on Linux. It is the
// host's, not the bundle's, so the bundle's pins cannot say which.
func (s *suite) nftRow(ctx context.Context, env []string) stackRow {
	cctx, cancel := context.WithTimeout(ctx, rmTimeout)
	defer cancel()
	out, stderr, code, err := s.exec(cctx, env, "nft", "--version")
	if err != nil || code != 0 {
		return stackRow{"nftables", "not read: nft --version: " + failure(stderr, code, err)}
	}
	return stackRow{"nftables", firstLine(out)}
}

// nerdctlAssets names the kernel and initrd a boot on Linux took. The bundle
// sets BRIG_BOOT_ASSETS to its own copy, built from the bundle KERNEL_SOURCE
// names. Without the variable brig fetches the bundle with oras and writes
// provenance.json beside it, which names what was fetched. The directory
// itself is left out: it is under the user's home.
func (s *suite) nerdctlAssets(env []string, rep *report) string {
	dir := envValue(env, "BRIG_BOOT_ASSETS")
	if dir != "" {
		if src := rep.pins["KERNEL_SOURCE"]; src != "" {
			// The bundle sets the variable to its own copy, but a user can
			// point it elsewhere, so this says what the bundle's copy is and
			// not that the boot took it.
			return "a local directory, from BRIG_BOOT_ASSETS. The runtime bundle's own copy is built from " + src
		}
		return "a local directory, from BRIG_BOOT_ASSETS"
	}
	if d := envValue(env, "XDG_DATA_HOME"); d != "" && filepath.IsAbs(d) {
		dir = filepath.Join(d, "brig", "assets")
	} else if home := envValue(env, "HOME"); home != "" {
		dir = filepath.Join(home, ".local", "share", "brig", "assets")
	} else {
		return "not read: no BRIG_BOOT_ASSETS, XDG_DATA_HOME or HOME"
	}
	b, err := os.ReadFile(filepath.Join(dir, "provenance.json"))
	if err != nil {
		return "not read: no provenance.json beside the kernel and initrd brig fetched"
	}
	var rec struct {
		Ref            string `json:"ref"`
		VerifiedDigest string `json:"verifiedDigest"`
	}
	if err := json.Unmarshal(b, &rec); err != nil || rec.Ref == "" {
		return "not read: provenance.json names no bundle"
	}
	if rec.VerifiedDigest != "" && !strings.Contains(rec.Ref, rec.VerifiedDigest) {
		return rec.Ref + ", verified " + rec.VerifiedDigest
	}
	return rec.Ref
}

// nerdctlDigest asks nerdctl which image the sandbox was created from, as
// hull inspect is asked on hvi. brig boots the digest it verified, so the
// container names the image by it and the store holds it under no tag: asking
// the store for the profile's tag finds nothing. A sandbox booted by tag, as
// one with nothing verified is, names the tag, and the store answers for it.
func (s *suite) nerdctlDigest(ctx context.Context, env []string) string {
	cctx, cancel := context.WithTimeout(ctx, rmTimeout)
	defer cancel()
	out, stderr, code, err := s.exec(cctx, env, s.runtime, "inspect", "--format", "{{.Image}}", sandboxName)
	if err != nil || code != 0 {
		return "not read: inspect: " + failure(stderr, code, err)
	}
	ref := firstLine(out)
	if _, digest, ok := strings.Cut(ref, "@"); ok && strings.HasPrefix(digest, "sha256:") {
		return digest
	}
	out, stderr, code, err = s.exec(cctx, env, s.runtime, "image", "inspect",
		"--format", "{{index .RepoDigests 0}}", ref)
	if err != nil || code != 0 {
		return "not read: image inspect: " + failure(stderr, code, err)
	}
	if _, digest, ok := strings.Cut(firstLine(out), "@"); ok && strings.HasPrefix(digest, "sha256:") {
		return digest
	}
	return "not read: image inspect names no digest"
}

// envValue is the value of key in env, the last one winning as it does for
// exec.
func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// nerdctlExitRE is nerdctl's report of a guest command that failed, which is
// the only place the command's own status survives on this path.
var nerdctlExitRE = regexp.MustCompile(`exec failed with exit code ([0-9]+)`)

// nerdctlExit is the guest command's exit status. nerdctl exits 1 for any
// command that failed (#469), and brig passes that on, so a probe that exited
// 3 would read as 1 and its line as something else's. nerdctl names the real
// status in its error line, which is read when brig exited 1. A status the
// line does not name is left as it is, and the probe line is still what
// decides the outcome: this only puts back the check that it agrees.
func nerdctlExit(stderr string, code int) int {
	if code != 1 {
		return code
	}
	m := nerdctlExitRE.FindAllStringSubmatch(stderr, -1)
	if len(m) == 0 {
		return code
	}
	n, err := strconv.Atoi(m[len(m)-1][1])
	if err != nil {
		return code
	}
	return n
}
