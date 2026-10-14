package runtime

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MoveReport is how a move went, read from hull's lines as they arrived.
// A field hull did not report is left out of the JSON rather than zero.
type MoveReport struct {
	// Rounds are the rounds of the copy, in the order hull reported them.
	Rounds []MoveRound `json:"rounds,omitempty"`
	// Seconds is how long hull's move took, start to end, as brig timed it.
	Seconds float64 `json:"seconds,omitempty"`
	// DowntimeSeconds is how long hull says the guest was paused.
	DowntimeSeconds float64 `json:"downtimeSeconds,omitempty"`
	// Bytes is what hull says it sent, when it says so in bytes.
	Bytes int64 `json:"bytes,omitempty"`
	// MiBPerSec is the rate hull reports for the whole copy.
	MiBPerSec float64 `json:"mibPerSec,omitempty"`
	// StepsMs are the milliseconds hull reports each step of the move
	// took, by hull's name for the step: fence, transfer, restore paused,
	// commit.
	StepsMs map[string]int64 `json:"stepsMs,omitempty"`
}

// MoveRound is one round of a move's copy.
type MoveRound struct {
	Round            int     `json:"round"`
	Bytes            int64   `json:"bytes,omitempty"`
	Seconds          float64 `json:"seconds,omitempty"`
	MiBPerSec        float64 `json:"mibPerSec,omitempty"`
	DirtyPagesPerSec float64 `json:"dirtyPagesPerSec,omitempty"`
}

// The parts of hull's lines a report reads. Each is matched on its own, so
// a line hull words differently still gives up the parts it has, and a line
// without them changes nothing. Bytes are read only where hull prints a
// count of bytes: its 1.7 GiB is rounded, and a count made from it would
// claim a precision hull did not give.
var (
	lineRound    = regexp.MustCompile(`(?i)^round (\d+)\b`)
	lineBytes    = regexp.MustCompile(`\b(\d+) bytes\b`)
	lineSeconds  = regexp.MustCompile(`\bin (\d+(?:\.\d+)?) ?s\b`)
	lineRate     = regexp.MustCompile(`\b(\d+(?:\.\d+)?) ?(KiB|MiB|GiB)/s\b`)
	lineDirty    = regexp.MustCompile(`\b(\d+(?:\.\d+)?) pages/s\b`)
	lineDowntime = regexp.MustCompile(`(?i)\bdowntime:? (\d+(?:\.\d+)?) ?(ms|s)\b`)
	lineSent     = regexp.MustCompile(`^Sent\b`)
	lineTimings  = regexp.MustCompile(`^Timings \(ms\): (.+)$`)
	lineStep     = regexp.MustCompile(`^([a-z][a-z ]*?) (\d+)$`)
)

// Add reads one line hull printed. A line it does not know changes nothing.
func (r *MoveReport) Add(line string) {
	line = strings.TrimSpace(line)
	if m := lineDowntime.FindStringSubmatch(line); m != nil {
		r.DowntimeSeconds = lineDuration(m[1], m[2])
	}
	if m := lineRound.FindStringSubmatch(line); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return
		}
		round := MoveRound{Round: n, Bytes: lineByteCount(line), Seconds: lineFloat(lineSeconds, line),
			MiBPerSec: lineMiBPerSec(line), DirtyPagesPerSec: lineFloat(lineDirty, line)}
		r.Rounds = append(r.Rounds, round)
		return
	}
	if m := lineTimings.FindStringSubmatch(line); m != nil {
		// hull's last line: "Timings (ms): fence 1326, transfer 4765,
		// restore paused 861, commit 489".
		for _, part := range strings.Split(m[1], ",") {
			if sm := lineStep.FindStringSubmatch(strings.TrimSpace(part)); sm != nil {
				ms, err := strconv.ParseInt(sm[2], 10, 64)
				if err != nil {
					continue
				}
				if r.StepsMs == nil {
					r.StepsMs = map[string]int64{}
				}
				r.StepsMs[sm[1]] = ms
			}
		}
		return
	}
	if lineSent.MatchString(line) {
		// hull's line for the whole copy: what it sent and how fast.
		if b := lineByteCount(line); b > 0 {
			r.Bytes = b
		}
		if rate := lineMiBPerSec(line); rate > 0 {
			r.MiBPerSec = rate
		}
	}
}

func lineByteCount(line string) int64 {
	m := lineBytes.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

func lineFloat(re *regexp.Regexp, line string) float64 {
	m := re.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	return f
}

// lineMiBPerSec is the rate on a line, in MiB/s.
func lineMiBPerSec(line string) float64 {
	m := lineRate.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	switch m[2] {
	case "KiB":
		return f / 1024
	case "GiB":
		return f * 1024
	}
	return f
}

func lineDuration(v, unit string) float64 {
	f, _ := strconv.ParseFloat(v, 64)
	if unit == "ms" {
		return f / 1000
	}
	return f
}

// Summary is the report in one line, for a person.
func (r *MoveReport) Summary() string {
	parts := []string{fmt.Sprintf("hull's move took %.1f s", r.Seconds)}
	if r.DowntimeSeconds > 0 {
		parts = append(parts, fmt.Sprintf("the guest was paused %.2f s", r.DowntimeSeconds))
	}
	if r.Bytes > 0 {
		parts = append(parts, fmt.Sprintf("%d bytes sent", r.Bytes))
	}
	if r.MiBPerSec > 0 {
		parts = append(parts, fmt.Sprintf("at %.1f MiB/s", r.MiBPerSec))
	}
	if n := len(r.Rounds); n > 0 {
		parts = append(parts, fmt.Sprintf("%d rounds", n))
	}
	// hull's steps in the order a move takes them, then any other.
	var steps []string
	seen := map[string]bool{}
	for _, name := range []string{"fence", "transfer", "restore paused", "commit"} {
		if ms, ok := r.StepsMs[name]; ok {
			steps, seen[name] = append(steps, fmt.Sprintf("%s %.2f s", name, float64(ms)/1000)), true
		}
	}
	var rest []string
	for name := range r.StepsMs {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		steps = append(steps, fmt.Sprintf("%s %.2f s", name, float64(r.StepsMs[name])/1000))
	}
	line := strings.Join(parts, ", ")
	if len(steps) > 0 {
		line += "; " + strings.Join(steps, ", ")
	}
	return line
}
