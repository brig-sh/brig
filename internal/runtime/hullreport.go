package runtime

import (
	"context"
	"encoding/json"
	"os/exec"
	"sync"
	"time"
)

// gatewayReport is what a hull says its network gateway can do, as `hull
// version --json` prints it under capabilities.gateway. hull only ever adds
// keys there, so a key this build of hull does not print reads as false.
type gatewayReport struct {
	EgressDefault   bool `json:"egressDefault"`
	Divert          bool `json:"divert"`
	DivertLinkLocal bool `json:"divertLinkLocal"`
}

// gatewayReports holds what each hull binary answered: a *gatewayReport, or
// nil for a hull that answered and has no report. An answer brig could not
// read is not kept, so the next question asks again.
var gatewayReports sync.Map

// gatewayReportOf returns the report of the hull at bin, and false when that
// hull has none.
//
// The report is a read-only question. Without it brig learns the same things
// from the gateway's help text, and for a link-local divert by starting the
// gateway with a flag it will refuse. A plan asks these before anything is
// approved, so the question that starts nothing is asked first.
//
// An answer brig could not read is asked for again by the next question, so
// a run that asks all three of a hull whose command fails runs it three
// times before the probes.
//
// False sends the caller to those probes, which is how every hull older than
// the report is still answered for. That covers a hull whose `version --json`
// fails, prints something that is not the document, or prints the document
// without capabilities.gateway: hull printed that document, with no
// capabilities, before it had a report. Only a gateway object that is there
// is believed, and then a key it lacks is a thing the gateway cannot do.
//
// It waits as long as a probe does, for the same reason: the first run of a
// new binary on macOS can wait on the system's security scan.
func gatewayReportOf(bin string) (*gatewayReport, bool) {
	return gatewayReportWithin(bin, gatewayProbeTimeout)
}

// gatewayReportWithin is gatewayReportOf with the wait the caller can
// afford. An answer that did not come in time is no report, and is not kept.
func gatewayReportWithin(bin string, wait time.Duration) (*gatewayReport, bool) {
	if v, ok := gatewayReports.Load(bin); ok {
		r, _ := v.(*gatewayReport)
		return r, r != nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "version", "--json")
	// Plumbing: hull sends no event of its own for a question brig asks. A
	// hull older than the report counts `version` like any other command
	// unless it is told not to.
	cmd.Env = mergeEnv(telemetryEnv(false))
	// See agentCallWaitDelay: without it a child the runtime left behind
	// holds the pipe open past the kill.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var doc struct {
		Version      string `json:"version"`
		Capabilities *struct {
			Gateway *gatewayReport `json:"gateway"`
		} `json:"capabilities"`
	}
	// version is in every document hull has printed, so output without it
	// is not hull's answer, and is not kept as one.
	if err := json.Unmarshal(out, &doc); err != nil || doc.Version == "" {
		return nil, false
	}
	var r *gatewayReport
	if doc.Capabilities != nil {
		r = doc.Capabilities.Gateway
	}
	gatewayReports.Store(bin, r)
	return r, r != nil
}

// How brig learns what a runtime's gateway can do, as GatewayAnswers
// returns it.
const (
	// GatewayByReport is a hull that says so in `version --json`.
	GatewayByReport = "report"
	// GatewayByProbe is a hull with no report, whose gateway brig probes.
	GatewayByProbe = "probe"
)

// GatewayAnswers says how brig learns what rt's gateway can do, for `brig
// doctor` to show. It is empty for a runtime brig starts no gateway for:
// anything but hull, and hull on Linux.
func GatewayAnswers(rt Runtime) string {
	h, ok := rt.(*hull)
	if !ok || h.linux {
		return ""
	}
	// With the bound Version gives the same binary, not a probe's: doctor is
	// what you run when something is already wrong, and a hull that has
	// wedged must not hold its row for half a minute.
	if _, ok := gatewayReportWithin(h.bin, versionTimeout); ok {
		return GatewayByReport
	}
	return GatewayByProbe
}
