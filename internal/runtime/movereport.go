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
	// Transport is how hull said it moved the guest: "stream", in rounds
	// while the guest ran, or "bundle", the whole checkpoint and disk with
	// the guest paused, for the reason in TransportWhy. Empty from a hull
	// that does not say. hull may change its mind before the guest stops,
	// when the base it sends first does not arrive, and the last word is
	// kept.
	Transport    string `json:"transport,omitempty"`
	TransportWhy string `json:"transportWhy,omitempty"`
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
	// Final is the round taken while the guest was paused: the disk round
	// hull names final, and the last memory round. hull names no memory
	// round final; hvi takes the last one in the pause, and the report
	// marks it so once hull says hvi sealed the stream.
	Final bool `json:"final,omitempty"`
	// Size is what hull says the round carried, as hull wrote it, and
	// Bytes the same when hull gave it as a count of bytes.
	Size    string  `json:"size,omitempty"`
	Bytes   int64   `json:"bytes,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	// MiBPerSec is the round's rate, and DirtyBlocksPerSec how fast the
	// guest wrote disk blocks during it, which hull prints for a disk
	// round after the first. hull prints no such rate for memory.
	MiBPerSec         float64 `json:"mibPerSec,omitempty"`
	DirtyBlocksPerSec float64 `json:"dirtyBlocksPerSec,omitempty"`
}

// The parts of hull's lines a report reads. Each is matched on its own, so
// a line hull words differently still gives up the parts it has, and a line
// without them changes nothing: brig passes every line on as hull printed
// it, and a line the report does not know is only printed. Bytes are read
// only where hull prints a count of bytes: its 1.7 GiB is rounded, and a
// count made from it would claim a precision hull did not give.
//
// hull 511877d prints a streamed move's rounds after "move: ", as
// "disk round 1: 163 blocks, 10.2 MiB (10.0 MiB on the wire) in 0.19 s,
// 53.5 MiB/s, guest wrote 52 blocks/s", "disk round 3 (final): ..." and
// "ram-round-0: 138.2 MiB in 2.56 s, 53.9 MiB/s", and the downtime as
// "downtime about 1450 ms". During a long round it prints how far the round
// has got, "disk round 0: 1574 blocks so far, 48.1 MiB/s", which is not a
// round of its own. Once hvi carries the disk rounds in its own stream, the
// rounds take hvi's component names, "disk-round-0" and on, and the one hvi
// takes in the pause is "disk-round-final"; a round named final in either
// spelling is numbered after the rounds of its kind before it.
var (
	lineRound     = regexp.MustCompile(`(?i)^(?:move: )?(?:(disk|ram)[ -])?(?:(final) )?round(?:[ -](\d+|final))?\b[^:]*:\s*(.*)$`)
	lineSize      = regexp.MustCompile(`^(?:\d+ blocks, )?(\d+(?:\.\d+)? (?:bytes|KiB|MiB|GiB))\b`)
	lineBytes     = regexp.MustCompile(`\b(\d+) bytes(?:[^/]|$)`)
	lineSeconds   = regexp.MustCompile(`\bin (\d+(?:\.\d+)?) ?s\b`)
	lineRate      = regexp.MustCompile(`\b(\d+(?:\.\d+)?) ?(bytes|KiB|MiB|GiB)/s\b`)
	lineBlocks    = regexp.MustCompile(`\b(\d+(?:\.\d+)?) blocks/s\b`)
	lineDowntime  = regexp.MustCompile(`(?i)\bdowntime:?(?: about)? (\d+(?:\.\d+)?) ?(ms|s)\b`)
	lineTransport = regexp.MustCompile(`^Transport: (?:(streamed)\b|the whole checkpoint and disk(?:, because (.*))?)`)
	lineSealed    = regexp.MustCompile(`^(?:move: )?hvi sealed the stream\b`)
	lineSent      = regexp.MustCompile(`^Sent\b`)
	lineSentN     = regexp.MustCompile(`^Sent (\d+) bytes\b`)
	lineTimings   = regexp.MustCompile(`^Timings \(ms\): (.+)$`)
	lineStep      = regexp.MustCompile(`^([a-z][a-z ]*?) (\d+)$`)
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
		named := strings.ToLower(m[3])
		round := MoveRound{Name: name, Kind: strings.ToLower(m[1]), Final: m[2] != "" || named == "final" ||
			strings.Contains(name, "(final)"), Bytes: lineByteCount(rest), Seconds: lineFloat(lineSeconds, rest),
			MiBPerSec: lineMiBPerSec(rest), DirtyBlocksPerSec: lineFloat(lineBlocks, rest)}
		if named != "" && named != "final" {
			round.Round, _ = strconv.Atoi(named)
		} else if n := len(r.Rounds); n > 0 && round.Final {
			// "disk final round" and "disk-round-final" follow the
			// numbered ones.
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
	if m := lineTransport.FindStringSubmatch(line); m != nil {
		// "Transport: streamed (pre-copy rounds; ...)", or "Transport: the
		// whole checkpoint and disk, because WHY".
		if m[1] != "" {
			r.Transport, r.TransportWhy = "stream", ""
		} else {
			r.Transport, r.TransportWhy = "bundle", m[2]
		}
		return
	}
	if lineSealed.MatchString(line) {
		// "move: hvi sealed the stream and fenced s (39 ms after the last
		// RAM round began)": the RAM rounds are over, and hvi took the
		// last one with the guest paused.
		r.markPausedRAM()
		return
	}
	if lineSent.MatchString(line) {
		// A hull that does not say it sealed has still ended the rounds.
		r.markPausedRAM()
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

// roundsSaid is the summary's part on the rounds: how many, of which kind,
// and the final disk round, the one the guest waited for.
func (r *MoveReport) roundsSaid(n int) string {
	var disk, ram int
	var final *MoveRound
	for i, rd := range r.Rounds {
		switch rd.Kind {
		case "disk":
			disk++
			if rd.Final {
				final = &r.Rounds[i]
			}
		case "ram":
			ram++
		}
	}
	said := fmt.Sprintf("%d rounds", n)
	if disk+ram == 0 {
		return said
	}
	said += fmt.Sprintf(" (%d disk, %d memory)", disk, ram)
	if final != nil && final.Size != "" {
		said += fmt.Sprintf(", the final disk round %s in %.2f s", final.Size, final.Seconds)
	}
	return said
}

// markPausedRAM marks the last memory round as the one taken in the pause.
// hvi stops the guest before its last round, whatever its size, so that
// round is the memory part of the downtime.
func (r *MoveReport) markPausedRAM() {
	for i := len(r.Rounds) - 1; i >= 0; i-- {
		if r.Rounds[i].Kind == "ram" {
			r.Rounds[i].Final = true
			return
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
	switch r.Transport {
	case "stream":
		parts = append(parts, "streamed")
	case "bundle":
		how := "the whole checkpoint and disk"
		if r.TransportWhy != "" {
			how += " (" + r.TransportWhy + ")"
		}
		parts = append(parts, how)
	}
	if r.DowntimeSeconds > 0 {
		parts = append(parts, fmt.Sprintf("hull estimates the guest was paused about %.2f s", r.DowntimeSeconds))
	}
	if r.Bytes > 0 {
		parts = append(parts, fmt.Sprintf("%d bytes sent", r.Bytes))
	}
	if r.MiBPerSec > 0 {
		parts = append(parts, fmt.Sprintf("at %.1f MiB/s", r.MiBPerSec))
	}
	if n := len(r.Rounds); n > 0 {
		parts = append(parts, r.roundsSaid(n))
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
