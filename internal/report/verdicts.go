package report

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// pattern is one known failure. match reads only what Collect already
// gathered; a pattern never runs anything of its own. Adding one is an entry
// in patterns and a test.
type pattern struct {
	name  string
	match func(r *Report) (evidence, fix string, ok bool)
}

var patterns = []pattern{
	{"oom", outOfMemory},
	{"thrash", thrash},
	{"unresponsive", unresponsive},
	{"gateway", gatewayDown},
	{"tmpfs", tmpfsFull},
}

// match returns the verdicts that apply, or a single "none".
func match(r *Report) []Verdict {
	var out []Verdict
	for _, p := range patterns {
		if evidence, fix, ok := p.match(r); ok {
			out = append(out, Verdict{Name: p.name, Evidence: evidence, Fix: fix})
		}
	}
	if len(out) == 0 {
		out = append(out, Verdict{Name: "none", Fix: "No known pattern matched."})
	}
	return out
}

var (
	ansi      = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	oomKilled = regexp.MustCompile(`Out of memory: Killed process ([^,]+)`)
)

// outOfMemory counts the guest kernel's kills in the console and in dmesg.
// The two overlap, so a kill is counted once, keyed by what the kernel says
// it killed.
func outOfMemory(r *Report) (string, string, bool) {
	text := r.Console
	if dmesg, ok := r.probe("dmesg"); ok {
		text += "\n" + dmesg
	}
	seen := map[string]bool{}
	last := ""
	for _, line := range strings.Split(ansi.ReplaceAllString(text, ""), "\n") {
		m := oomKilled.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		last = strings.TrimSpace(line)
		seen[m[1]] = true
	}
	r.OOMKills = len(seen)
	if r.OOMKills == 0 {
		return "", "", false
	}
	kills := "1 kill"
	if r.OOMKills > 1 {
		kills = fmt.Sprintf("%d kills", r.OOMKills)
	}
	return last, fmt.Sprintf("The guest ran out of memory (%s). %s", kills, r.memFix()), true
}

// thrashAt is the share of the last ten seconds in which every task in the
// guest waited on memory, above which a guest that has not yet killed
// anything is called thrashing. Ten per cent is already far from a healthy
// guest, which sits at zero.
const thrashAt = 10.0

var fullAvg10 = regexp.MustCompile(`full avg10=([0-9.]+)`)

func thrash(r *Report) (string, string, bool) {
	if r.OOMKills > 0 {
		return "", "", false
	}
	psi, ok := r.probe("pressure")
	if !ok {
		return "", "", false
	}
	_, memory, found := strings.Cut(psi, "memory:")
	if !found {
		return "", "", false
	}
	if next := strings.Index(memory, ":\n"); next >= 0 {
		memory = memory[:strings.LastIndex(memory[:next], "\n")+1]
	}
	for _, line := range strings.Split(memory, "\n") {
		m := fullAvg10.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[1], 64)
		if err == nil && v >= thrashAt {
			return "memory " + strings.TrimSpace(line),
				"The guest is short of memory and thrashing. " + r.memFix(), true
		}
	}
	return "", "", false
}

func (r *Report) memFix() string {
	ref := r.in.Ref
	if ref == "" {
		ref = "<ref>"
	}
	fix := fmt.Sprintf("Stop the sandbox with brig stop %s, then run it with a larger --mem.", ref)
	if r.HaveVM && r.VM.MemMiB > 0 {
		fix += fmt.Sprintf(" It has %d MB.", r.VM.MemMiB)
	}
	return fix
}

// unresponsive says whether the VMM was busy when the guest went silent: a
// busy VMM points at the guest, an idle one at the VMM.
func unresponsive(r *Report) (string, string, bool) {
	if len(r.Probes) == 0 || !r.Probes[0].TimedOut {
		return "", "", false
	}
	fix := fmt.Sprintf("The guest did not answer within %s.", r.Probes[0].Elapsed.Round(100*time.Millisecond))
	vmm := "the VMM"
	if r.HaveVM && r.VM.Backend != "" {
		vmm = r.VM.Backend
	}
	if r.haveCPU {
		if r.vmmCPU < 1 {
			fix += " " + vmm + " is idle."
		} else {
			fix += fmt.Sprintf(" %s is using %.1f%% CPU.", vmm, r.vmmCPU)
		}
	}
	return "probe " + r.Probes[0].Name + " timed out", fix, true
}

func gatewayDown(r *Report) (string, string, bool) {
	if r.in.Network != "isolated" || r.in.GatewayPIDFile == "" || !r.in.Running {
		return "", "", false
	}
	blob, err := os.ReadFile(r.in.GatewayPIDFile)
	if errors.Is(err, fs.ErrNotExist) && !r.in.GatewayAlways {
		return "", "", false
	}
	if err != nil {
		return r.in.GatewayPIDFile + ": " + err.Error(), "The network gateway for this sandbox is not running.", true
	}
	pid, err := strconv.Atoi(strings.Fields(string(blob) + " 0")[0])
	if err != nil || pid <= 0 || !alive(pid) {
		return fmt.Sprintf("gateway pid %s is not running", strings.TrimSpace(string(blob))),
			"The network gateway for this sandbox is not running.", true
	}
	return "", "", false
}

func tmpfsFull(r *Report) (string, string, bool) {
	df, ok := r.probe("df -h")
	if !ok {
		return "", "", false
	}
	for _, line := range strings.Split(df, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[len(f)-1] == "/brig/claude" && f[len(f)-2] == "100%" {
			return strings.TrimSpace(line), "The agent's config tmpfs is full.", true
		}
	}
	return "", "", false
}
