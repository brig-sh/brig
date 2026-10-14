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

// MoveRound is one round of a move's copy: of the guest's memory, of its
// disk, or of a copy hull did not say which.
type MoveRound struct {
	// Name is hull's name for the round: "disk round 1", "ram-round-0".
	Name  string `json:"name"`
	Kind  string `json:"kind,omitempty"`
	Round int    `json:"round"`
	// Final is the round taken while the guest was paused.
	Final bool `json:"final,omitempty"`
	// Size is what hull says the round carried, as hull wrote it, and
	// Bytes the same when hull gave it as a count of bytes.
	Size    string  `json:"size,omitempty"`
	Bytes   int64   `json:"bytes,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	// MiBPerSec is the round's rate, and the dirty rates how fast the
	// guest wrote memory pages or disk blocks during it.
	MiBPerSec         float64 `json:"mibPerSec,omitempty"`
	DirtyPagesPerSec  float64 `json:"dirtyPagesPerSec,omitempty"`
	DirtyBlocksPerSec float64 `json:"dirtyBlocksPerSec,omitempty"`
}

// The parts of hull's lines a report reads. Each is matched on its own, so
// a line hull words differently still gives up the parts it has, and a line
// without them changes nothing. Bytes are read only where hull prints a
// count of bytes: its 1.7 GiB is rounded, and a count made from it would
// claim a precision hull did not give.
//
// hull prints a streamed move's rounds after "move: ", as
// "disk round 1: 12 blocks, 768.0 KiB (301.2 KiB on the wire) in 0.20 s,
// 1.5 MiB/s, guest wrote 60 blocks/s" and "ram-round-0: 90.7 MiB in 2.10 s,
// 43.2 MiB/s", and the downtime as "downtime about 1450 ms". During a long
// round it prints how far the round has got, "disk round 0: 812 blocks so
// far, 30.1 MiB/s (34% of 2381)", which is not a round of its own.
var (
	lineRound    = regexp.MustCompile(`(?i)^(?:move: )?(?:(disk|ram)[ -])?(?:(final) )?round(?:[ -](\d+))?\b[^:]*:\s*(.*)$`)
	lineSize     = regexp.MustCompile(`^(?:\d+ blocks, )?(\d+(?:\.\d+)? (?:bytes|KiB|MiB|GiB))\b`)
	lineBytes    = regexp.MustCompile(`\b(\d+) bytes(?:[^/]|$)`)
	lineSeconds  = regexp.MustCompile(`\bin (\d+(?:\.\d+)?) ?s\b`)
	lineRate     = regexp.MustCompile(`\b(\d+(?:\.\d+)?) ?(bytes|KiB|MiB|GiB)/s\b`)
	lineDirty    = regexp.MustCompile(`\b(\d+(?:\.\d+)?) pages/s\b`)
	lineBlocks   = regexp.MustCompile(`\b(\d+(?:\.\d+)?) blocks/s\b`)
	lineDowntime = regexp.MustCompile(`(?i)\bdowntime:?(?: about)? (\d+(?:\.\d+)?) ?(ms|s)\b`)
	lineSent     = regexp.MustCompile(`^Sent\b`)
	lineSentN    = regexp.MustCompile(`^Sent (\d+) bytes\b`)
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
		name, _, _ := strings.Cut(strings.TrimPrefix(line, "move: "), ":")
		rest := m[4]
		if strings.Contains(rest, " so far") {
			// hull's line during a long round ("disk round 0: 812 blocks
			// so far, 30.1 MiB/s (34% of 2381)"): the round has not ended.
			return
		}
		round := MoveRound{Name: name, Kind: strings.ToLower(m[1]), Final: m[2] != "" ||
			strings.Contains(name, "(final)"), Bytes: lineByteCount(rest), Seconds: lineFloat(lineSeconds, rest),
			MiBPerSec: lineMiBPerSec(rest), DirtyPagesPerSec: lineFloat(lineDirty, rest),
			DirtyBlocksPerSec: lineFloat(lineBlocks, rest)}
		if m[3] != "" {
			round.Round, _ = strconv.Atoi(m[3])
		} else if n := len(r.Rounds); n > 0 && round.Final {
			// "disk final round" follows the numbered ones.
			for _, prev := range r.Rounds {
				if prev.Kind == round.Kind && prev.Round >= round.Round {
					round.Round = prev.Round + 1
				}
			}
		}
		if sm := lineSize.FindStringSubmatch(rest); sm != nil {
			round.Size = sm[1]
		}
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
		// hull's line for the whole copy: what it sent and how fast. Only
		// the count right after "Sent" is the total; a streamed move's line
		// goes on with the parts, "state 53259 bytes" among them.
		if m := lineSentN.FindStringSubmatch(line); m != nil {
			r.Bytes, _ = strconv.ParseInt(m[1], 10, 64)
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
	case "bytes":
		return f / (1 << 20)
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
	// hull's steps in the order a move takes them, then any other. A
	// streamed move has no separate fence or restore, and hull reports
	// them as 0; the JSON keeps them, the line leaves them out.
	var steps []string
	seen := map[string]bool{}
	for _, name := range []string{"fence", "transfer", "restore paused", "commit"} {
		if ms, ok := r.StepsMs[name]; ok && ms > 0 {
			steps = append(steps, fmt.Sprintf("%s %.2f s", name, float64(ms)/1000))
		}
		seen[name] = true
	}
	var rest []string
	for name, ms := range r.StepsMs {
		if !seen[name] && ms > 0 {
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
