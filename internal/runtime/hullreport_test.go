package runtime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reportHull writes a stand-in for hull that answers `version --json` with
// versionOut and exit status versionExit, answers `network-gateway --help`
// with help, and refuses a link-local divert the way an old hull does. Every
// call is written to the log it returns, one line of arguments each.
func reportHull(t *testing.T, versionOut string, versionExit int, help string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "hull"), filepath.Join(dir, "argv")
	script := "#!/bin/sh\n" +
		"echo \"$* suppress=${HULL_TELEMETRY_SUPPRESS-unset}\" >> '" + log + "'\n" +
		"case \"$1\" in\n" +
		"version)\ncat <<'EOF'\n" + versionOut + "\nEOF\nexit " + itoa(versionExit) + " ;;\n" +
		"network-gateway)\n" +
		"  case \" $* \" in *' --help '*)\ncat <<'EOF'\n" + help + "\nEOF\n  exit 0 ;; esac\n" +
		"  echo 'a divert of a link-local address is refused' >&2; exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// calls returns the lines of log that start with verb.
func calls(t *testing.T, log, verb string) []string {
	t.Helper()
	blob, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(blob), "\n") {
		if strings.HasPrefix(line, verb) {
			out = append(out, line)
		}
	}
	return out
}

const (
	helpWithFlags = "   --egress-default string   verdict\n   --divert host:port=socket   divert"
	helpWithout   = "   --socket string   control socket path"
)

// A hull that reports what its gateway can do is believed, and the gateway
// is never started to find out. The stand-in's help says the opposite of its
// report, so an answer taken from a probe would fail here.
func TestAHullWithAReportIsAskedNoProbe(t *testing.T) {
	bin, log := reportHull(t, `{"version": "v0.2.0", "capabilities": {"gateway":
		{"api": true, "egressDefault": true, "divert": true, "divertLinkLocal": true, "later": true}}}`, 0, helpWithout)
	for name, ask := range map[string]func(string) error{
		"gatewayEnforces": gatewayEnforces, "gatewayDiverts": gatewayDiverts, "probeLinkLocal": probeLinkLocal,
	} {
		if err := ask(bin); err != nil {
			t.Errorf("%s refused a hull whose report says it can: %v", name, err)
		}
	}
	if got := calls(t, log, "network-gateway"); len(got) != 0 {
		t.Errorf("a hull with a report was probed: %q", got)
	}
	if got := calls(t, log, "version"); len(got) != 1 {
		t.Errorf("the report was asked for %d times, want once per binary: %q", len(got), got)
	}
}

// What the report says the gateway cannot do is refused as cannot enforce,
// with no second opinion from a probe. A key the report lacks is one the
// gateway cannot do: hull only adds keys.
func TestAReportThatSaysNoIsARefusal(t *testing.T) {
	for name, doc := range map[string]string{
		"false":   `{"version": "v0.2.0", "capabilities": {"gateway": {"egressDefault": false, "divert": false, "divertLinkLocal": false}}}`,
		"missing": `{"version": "v0.2.0", "capabilities": {"gateway": {"api": true}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			bin, log := reportHull(t, doc, 0, helpWithFlags)
			for q, ask := range map[string]func(string) error{
				"gatewayEnforces": gatewayEnforces, "gatewayDiverts": gatewayDiverts, "probeLinkLocal": probeLinkLocal,
			} {
				var ce *CapabilityError
				if err := ask(bin); !errors.As(err, &ce) || ce.State != CannotEnforce {
					t.Errorf("%s = %v, want cannot enforce", q, err)
				}
			}
			if got := calls(t, log, "network-gateway"); len(got) != 0 {
				t.Errorf("a hull whose report said no was probed: %q", got)
			}
		})
	}
}

// A hull with no report is asked as before. hull printed the version
// document with no capabilities before it had a report, so that shape, a
// document with no gateway in it, a failing command and output that is not
// the document all go to the probes, and the probes' answers stand.
func TestAHullWithNoReportIsProbed(t *testing.T) {
	for name, c := range map[string]struct {
		out  string
		exit int
	}{
		"the document before the report": {`{"version": "v0.1.0", "os": "darwin"}`, 0},
		"capabilities with no gateway":   {`{"version": "v0.2.0", "capabilities": {}}`, 0},
		"a command that fails":           {`{"version": "v0.2.0", "capabilities": {"gateway": {"egressDefault": true}}}`, 2},
		"help text":                      {"USAGE: hull [command]", 0},
		"another JSON document":          {`{"capabilities": {"gateway": {"egressDefault": true, "divert": true}}}`, 0},
	} {
		t.Run(name, func(t *testing.T) {
			bin, log := reportHull(t, c.out, c.exit, helpWithFlags)
			if err := gatewayEnforces(bin); err != nil {
				t.Errorf("gatewayEnforces refused a hull whose help lists the flag: %v", err)
			}
			if err := gatewayDiverts(bin); err != nil {
				t.Errorf("gatewayDiverts refused a hull whose help lists the flag: %v", err)
			}
			var ce *CapabilityError
			if err := probeLinkLocal(bin); !errors.As(err, &ce) || ce.State != CannotEnforce {
				t.Errorf("probeLinkLocal = %v, want the probe's cannot enforce", err)
			}
			if got := calls(t, log, "network-gateway"); len(got) != 3 {
				t.Errorf("want the three probes, got %q", got)
			}
			if _, ok := gatewayReportOf(bin); ok {
				t.Error("a hull with no report was read as having one")
			}
		})
	}

	old, log := reportHull(t, `{"version": "v0.1.0"}`, 0, helpWithout)
	var ce *CapabilityError
	if err := gatewayEnforces(old); !errors.As(err, &ce) || ce.State != CannotEnforce {
		t.Errorf("gatewayEnforces = %v, want the probe's cannot enforce", err)
	}
	if got := calls(t, log, "network-gateway --help"); len(got) != 1 {
		t.Errorf("an old hull was not probed: %q", got)
	}
}

// An answer brig could not read is not kept. A hull that fails once, under
// load say, is asked again, and is not probed for the rest of the process.
func TestAnUnreadableReportIsAskedForAgain(t *testing.T) {
	bin, log := reportHull(t, "not json", 0, helpWithFlags)
	for range 2 {
		if _, ok := gatewayReportOf(bin); ok {
			t.Fatal("output that is not the document was read as a report")
		}
	}
	if got := calls(t, log, "version"); len(got) != 2 {
		t.Errorf("version was asked %d times, want twice", len(got))
	}
}

// brig doctor shows how the gateway is asked: by the report, or by probes
// for a hull that has none. A runtime brig starts no gateway for, hull on
// Linux among them, has no answer to show.
func TestGatewayAnswersSaysWhichWayAHullIsAsked(t *testing.T) {
	with, _ := reportHull(t, `{"version": "v0.2.0", "capabilities": {"gateway": {"egressDefault": true}}}`, 0, helpWithFlags)
	without, _ := reportHull(t, `{"version": "v0.1.0"}`, 0, helpWithFlags)
	for name, c := range map[string]struct {
		rt   Runtime
		want string
	}{
		"a hull with a report": {&hull{bin: with}, GatewayByReport},
		"a hull with none":     {&hull{bin: without}, GatewayByProbe},
		"hull on Linux":        {&hull{bin: with, linux: true}, ""},
		"no runtime":           {nil, ""},
	} {
		if got := GatewayAnswers(c.rt); got != c.want {
			t.Errorf("%s: GatewayAnswers = %q, want %q", name, got, c.want)
		}
	}
}

// The question and the probes are brig's plumbing, so hull is told to send
// no event of its own for any of them. A hull older than the report counts
// `version` as a command unless it is told not to.
func TestTheCapabilityQuestionsSendNoHullEvent(t *testing.T) {
	t.Setenv("HULL_TELEMETRY_SUPPRESS", "")
	for name, doc := range map[string]string{
		"a hull with a report": `{"version": "v0.2.0", "capabilities": {"gateway": {"egressDefault": true, "divert": true, "divertLinkLocal": true}}}`,
		"a hull with none":     `{"version": "v0.1.0"}`,
	} {
		bin, log := reportHull(t, doc, 0, helpWithFlags)
		_ = gatewayEnforces(bin)
		_ = gatewayDiverts(bin)
		_ = probeLinkLocal(bin)
		lines := calls(t, log, "")
		if len(lines) == 0 {
			t.Fatalf("%s was asked nothing", name)
		}
		for _, line := range lines {
			if line != "" && !strings.HasSuffix(line, "suppress=1") {
				t.Errorf("%s: hull was left free to count %q", name, line)
			}
		}
	}
}

// brig doctor gives the report the bound it gives --version. A hull that has
// wedged is no report, and the row does not wait a probe's half minute.
func TestGatewayAnswersDoesNotWaitAProbesBoundOnAWedgedHull(t *testing.T) {
	bin := scriptHull(t, "[ \"$1\" = version ] && exec sleep 60\nexit 0\n")
	if out, err := exec.Command(bin).CombinedOutput(); err != nil {
		t.Fatalf("the stub runtime did not run: %v: %s", err, out)
	}
	start := time.Now()
	got := GatewayAnswers(&hull{bin: bin})
	if took := time.Since(start); took > versionTimeout+10*time.Second {
		t.Errorf("GatewayAnswers took %s on a wedged hull, want about %s", took, versionTimeout)
	}
	if got != GatewayByProbe {
		t.Errorf("GatewayAnswers = %q for a hull that never answered", got)
	}
}
