package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The prepare's line names the destination, the store and the hull there,
// asks for hull's report as JSON, and names the sandbox last, as the move's
// does. A destination ssh could read as an option, or a sandbox name hull
// could read as a flag, is refused before any line is built.
func TestPrepareArgs(t *testing.T) {
	for _, c := range []struct {
		what string
		spec PrepareSpec
		want string
	}{
		{"a prepare", PrepareSpec{}, "migrate-prepare --to me@box --json s"},
		{"into a named store, with the hull there", PrepareSpec{RemoteStore: "/st/b", RemoteHull: "/opt/hull"},
			"migrate-prepare --to me@box --remote-store /st/b --remote-hull /opt/hull --json s"},
		{"with the kernel pinned", PrepareSpec{RemoteStore: "/st/b", KernelSHA256: "sha256:7787"},
			"migrate-prepare --to me@box --remote-store /st/b --kernel-sha256 sha256:7787 --json s"},
	} {
		got, err := prepareArgs("s", "me@box", c.spec)
		if err != nil || strings.Join(got, " ") != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.what, strings.Join(got, " "), err, c.want)
		}
	}
	for _, bad := range []struct{ name, to string }{
		{"s", "-oProxyCommand=sh@box"}, {"s", "box"}, {"s", "me@box;reboot"}, {"", "me@box"}, {"--to", "me@box"},
	} {
		if got, err := prepareArgs(bad.name, bad.to, PrepareSpec{}); err == nil {
			t.Errorf("%q on %q was accepted as %q", bad.name, bad.to, got)
		}
	}
}

// hullPrepareReport is what `hull migrate-prepare --json` prints for a
// prepare of s on me@box that sent the kernel and the base and found the
// image there (cmd/hull/migrate_prepare.go, prepareResult).
const hullPrepareReport = `{"name":"s","to":"me@box","steps":[` +
	`{"what":"image 1a2b3c4d5e6f","result":"held","seconds":0.4},` +
	`{"what":"portable-arm64-v1 kernel","result":"sent","detail":"e85e4338ffff","bytes":30000000,"seconds":1.2},` +
	`{"what":"base 9f8e7d6c5b4a","result":"sent","detail":"1.6 GiB of data as 812.0 MiB, 30.1 s, 27.0 MiB/s","bytes":851443712,"seconds":30.1}],` +
	`"holds":{"hullVersion":"x","bases":["0000000000000000000000000000000000000000000000000000000000000000",` +
	`"9f8e7d6c5b4a00000000000000000000000000000000000000000000000000ff"],` +
	`"profileKernels":{"portable-arm64-v1":"e85e4338ffff0000000000000000000000000000000000000000000000000000"},"hviStream":true},` +
	`"stream":true}`

// prepareHull is a hull that logs every argv and the transport it was
// handed, prints two lines on stderr as a prepare does, and the report on
// stdout.
func prepareHull(t *testing.T, report string) (*hull, string) {
	t.Helper()
	dir := t.TempDir()
	bin, log := filepath.Join(dir, "hull"), filepath.Join(dir, "argv.log")
	rep := filepath.Join(dir, "report.json")
	if err := os.WriteFile(rep, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$* transport=${HULL_MIGRATE_SSH:-none}\" >> " + log + "\n" +
		"printf 'Preparing me@box for s\\n  base 9f8e7d6c5b4a: sent (1.6 GiB)\\n' >&2\n" +
		"cat " + rep + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &hull{bin: bin}, log
}

// The seam runs hull's prepare with the line prepareArgs builds and no other
// transport, hands its stderr on line by line, and reads its report: every
// step, the whole digest of the base the destination now holds (hull's step
// names only its head), the kernels there, and whether a move streams.
func TestHullPrepareSeam(t *testing.T) {
	t.Setenv("HULL_MIGRATE_SSH", "/tmp/elsewhere")
	h, log := prepareHull(t, hullPrepareReport)
	var lines []string
	p, err := h.MigratePrepare("s", "me@box", PrepareSpec{RemoteStore: "/st", KernelSHA256: "sha256:7787",
		Progress: func(l string) { lines = append(lines, l) }})
	if err != nil {
		t.Fatal(err)
	}
	// It asks first whether this hull takes --kernel-sha256; this one's
	// help lists no such flag, so the pin the caller set is not sent.
	if got, _ := os.ReadFile(log); string(got) != "migrate-prepare --help transport=/tmp/elsewhere\n"+
		"migrate-prepare --to me@box --remote-store /st --json s transport=none\n" {
		t.Errorf("hull was asked %q", got)
	}
	if strings.Join(lines, "|") != "Preparing me@box for s|base 9f8e7d6c5b4a: sent (1.6 GiB)" {
		t.Errorf("lines %q", lines)
	}
	if !p.Stream || len(p.Steps) != 3 || p.Steps[2].Bytes != 851443712 || p.Steps[1].Result != "sent" ||
		p.Base != "9f8e7d6c5b4a00000000000000000000000000000000000000000000000000ff" ||
		!strings.HasPrefix(p.Kernels["portable-arm64-v1"], "e85e4338ffff") {
		t.Errorf("report read as %+v", p)
	}
}

// A hull whose prepare lists --kernel-sha256 in its help is handed the
// portable kernel brig pins, so that a destination holding another one gets
// the pinned one. It is asked once, however many prepares it runs.
func TestHullPrepareHandsThePinToAHullThatTakesIt(t *testing.T) {
	dir := t.TempDir()
	bin, log := filepath.Join(dir, "hull"), filepath.Join(dir, "argv.log")
	rep := filepath.Join(dir, "report.json")
	if err := os.WriteFile(rep, []byte(hullPrepareReport), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n" +
		"case \"$*\" in *--help*) printf '   --json           print JSON\\n   --kernel-sha256 string  the sha256\\n'; exit 0;; esac\n" +
		"cat " + rep + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &hull{bin: bin}
	for range 2 {
		if _, err := h.MigratePrepare("s", "me@box", PrepareSpec{}); err != nil {
			t.Fatal(err)
		}
	}
	// The second prepare does not ask again: the answer cannot change.
	line := "migrate-prepare --to me@box --kernel-sha256 " + PortableKernelDigest + " --json s\n"
	want := "migrate-prepare --help\n" + line + line
	if got, _ := os.ReadFile(log); string(got) != want {
		t.Errorf("hull was asked %q, want %q", got, want)
	}
}

// A report that does not read, or that is about another sandbox or host, is
// an error, not an empty report: brig would otherwise record a host as
// ready that hull said nothing about. A base the step did not hold or send
// is not claimed, whatever else the destination holds.
func TestAPrepareReportIsReadOrRefused(t *testing.T) {
	for _, bad := range []string{"", "Prepared.", strings.Replace(hullPrepareReport, `"name":"s"`, `"name":"t"`, 1),
		strings.Replace(hullPrepareReport, `"to":"me@box"`, `"to":"you@box"`, 1)} {
		if p, err := preparedOf("s", "me@box", bad); err == nil {
			t.Errorf("%q read as %+v", bad, p)
		}
	}
	failed := strings.Replace(hullPrepareReport, `"what":"base 9f8e7d6c5b4a","result":"sent"`,
		`"what":"base 9f8e7d6c5b4a","result":"failed"`, 1)
	if p, err := preparedOf("s", "me@box", failed); err != nil || p.Base != "" {
		t.Errorf("a base that failed was claimed: %+v, %v", p, err)
	}
}
