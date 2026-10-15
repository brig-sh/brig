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
	// finalDiskSize and finalDiskMs are the final disk round in hull's
	// last line, "last disk round 6.3 MiB in 21 ms": its raw size, as hull
	// wrote it, and hvi's time for it, when finalDiskTimed says hull gave
	// one. The two are one pair, hvi's account of the round. The round's
	// own line, from hull d2bb5e9 on, has neither: it gives what the round
	// came to in hvi's stream and how long hull took to pass it on. A hull
	// older than d2bb5e9 gives only the time, "last disk round 3 ms".
	finalDiskSize  string
	finalDiskMs    int64
	finalDiskTimed bool
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
	// Bytes the same when hull gave it as a count of bytes. For a disk
	// round that is its raw size, which only a line that also says what
	// went on the wire gives, or, for the final disk round, hull's last
	// line, "last disk round 6.3 MiB in 21 ms"; Ms is the time that line
	// gives with it. Sent is what a disk round was sent as: the
	// part "on the wire" of such a line, or the one size of a disk round's
	// line from hull d2bb5e9 on, "disk-round-final: 161172 bytes in
	// 0.01 s", which is what the round came to in hvi's stream (hull's
	// framing adds a little on the wire). SentBytes is Sent as a count of
	// bytes.
	Size      string  `json:"size,omitempty"`
	Bytes     int64   `json:"bytes,omitempty"`
	Sent      string  `json:"sent,omitempty"`
	SentBytes int64   `json:"sentBytes,omitempty"`
	Seconds   float64 `json:"seconds,omitempty"`
	// Ms is the final disk round's time in whole milliseconds as hull's
	// last line gives it: hvi's time for the round from hull d2bb5e9 on,
	// not Seconds, which is how long hull took to pass the round on. 0
	// is a round shorter than a millisecond, so Ms is nil, not 0, when
	// hull gave no time.
	Ms *int64 `json:"ms,omitempty"`
	// took is the round's time as hull wrote it, "0.16 s" or "3 ms", or
	// "" when its line gave none: Seconds is 0 both for hull's "in 0.00 s"
	// and for a line with no time.
	took string
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
// round of its own. Since hull d2bb5e9 hvi carries the disk rounds in its
// own stream, and the rounds take hvi's component names, "disk-round-0" and
// on, and the one hvi takes in the pause is "disk-round-final"; a round
// named final in either spelling is numbered after the rounds of its kind
// before it. Such a disk round's line gives one size, what the round came
// to in hvi's stream, "disk-round-final: 161172 bytes in 0.01 s, 14.5
// MiB/s", and hull's last line gives its raw size and hvi's time for it,
// "last disk round 6.3 MiB in 21 ms".
var (
	lineRound     = regexp.MustCompile(`(?i)^(?:move: )?(?:(disk|ram)[ -])?(?:(final) )?round(?:[ -](\d+|final))?\b[^:]*:\s*(.*)$`)
	lineSize      = regexp.MustCompile(`^(?:\d+ blocks, )?(\d+(?:\.\d+)? (?:bytes|KiB|MiB|GiB))\b`)
	lineBytes     = regexp.MustCompile(`\b(\d+) bytes(?:[^/]|$)`)
	lineSeconds   = regexp.MustCompile(`\bin (\d+(?:\.\d+)?) ?(ms|s)\b`)
	lineFinalDisk = regexp.MustCompile(`\blast disk round (?:([^;]*?) in )?(\d+) ms\b`)
	lineOnWire    = regexp.MustCompile(`\((\d+(?:\.\d+)? (?:bytes|KiB|MiB|GiB)) on the wire\)`)
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
	if m := lineFinalDisk.FindStringSubmatch(line); m != nil {
		// hull's last line: "...; sealed 5087 ms after the pause, last
		// disk round 6.3 MiB in 21 ms; downtime about 7958 ms", or "last
		// disk round 3 ms" from a hull older than d2bb5e9.
		r.finalDiskSize = strings.TrimSpace(m[1])
		r.finalDiskMs, _ = strconv.ParseInt(m[2], 10, 64)
		r.finalDiskTimed = true
		r.fillFinalDisk()
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
			strings.Contains(name, "(final)"), Bytes: lineByteCount(rest),
			MiBPerSec: lineMiBPerSec(rest), DirtyBlocksPerSec: lineFloat(lineBlocks, rest)}
		if tm := lineSeconds.FindStringSubmatch(rest); tm != nil {
			round.Seconds, round.took = lineDuration(tm[1], tm[2]), tm[1]+" "+tm[2]
		}
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
		if round.Kind == "disk" {
			// A disk round's raw size is on its line only beside what
			// went on the wire. A line with one size, as hull d2bb5e9's,
			// gives what was sent, and nothing of the raw size.
			if wm := lineOnWire.FindStringSubmatch(rest); wm != nil {
				round.Sent = wm[1]
			} else {
				round.Sent, round.Size, round.Bytes = round.Size, "", 0
			}
			round.SentBytes = lineByteCount(round.Sent)
		}
		r.Rounds = append(r.Rounds, round)
		r.fillFinalDisk()
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
// and the final disk round, the one the guest waited for. Its size and time
// are said only as one pair hull gave: the raw size and hvi's time from
// hull's last line, unless that is only hull's default (finalDiskPair), or
// else the size and time on the round's own line. What it was sent as
// follows apart. A size or time hull did not give is left
// out, and the sent bytes are never given the raw round's time.
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
	if final == nil {
		return said
	}
	size, took := final.Size, roundTook(final.took, final.Seconds)
	lastSize, lastMs, timed := r.finalDiskPair(final)
	if timed {
		took = "in " + strconv.FormatInt(lastMs, 10) + " ms"
		if lastMs == 0 {
			// hvi counts whole milliseconds: 0 is a round shorter than
			// one, not none.
			took = "in under 1 ms"
		}
		if lastSize != "" {
			size = lastSize
		}
	}
	var of []string
	switch {
	case size == "" && !timed && final.Sent != "":
		// The round's own line, hull d2bb5e9's, with no last line to
		// give the raw size: its time is the time to pass on what was
		// sent.
		of = append(of, "sent as "+final.Sent)
		if took != "" {
			of = append(of, took)
		}
	default:
		for _, part := range []string{size, took} {
			if part != "" {
				of = append(of, part)
			}
		}
		if final.Sent != "" {
			of = append(of, "(sent as "+final.Sent+")")
		}
	}
	if len(of) > 0 {
		said += ", the final disk round " + strings.Join(of, " ")
	}
	return said
}

// finalDiskPair is the raw size and the time hull's last line gave for the
// final disk round, final, and whether the report has them. It has none
// without that line, and none from hull's default for it: hull prints
// "last disk round 0 bytes in 0 ms" when hvi's reply has no figures for a
// disk round taken in the pause, so 0 bytes in 0 ms beside a round line
// that sent something is no account of that round, and the summary would
// give a raw size of 0 next to a sent size that is not.
func (r *MoveReport) finalDiskPair(final *MoveRound) (size string, ms int64, ok bool) {
	if !r.finalDiskTimed {
		return "", 0, false
	}
	if r.finalDiskSize == "0 bytes" && r.finalDiskMs == 0 && final != nil && final.Sent != "" &&
		final.Sent != "0 bytes" {
		return "", 0, false
	}
	return r.finalDiskSize, r.finalDiskMs, true
}

// fillFinalDisk puts what hull's last line gave for the final disk round on
// that round, so that --json carries the pair the summary says: the raw
// size in Size, and in Bytes when it is a count of bytes, where the
// round's own line gave none, and the time in Ms. hull prints its last
// line after the rounds, but the report fills the round whichever comes
// first.
func (r *MoveReport) fillFinalDisk() {
	for i := len(r.Rounds) - 1; i >= 0; i-- {
		rd := &r.Rounds[i]
		if rd.Kind != "disk" || !rd.Final {
			continue
		}
		size, ms, ok := r.finalDiskPair(rd)
		if !ok {
			return
		}
		if rd.Size == "" && size != "" {
			rd.Size, rd.Bytes = size, lineByteCount(size)
		}
		rd.Ms = &ms
		return
	}
}

// roundTook is a round's time as its own line gave it, as took and secs
// hold it, for the summary. hull's "in 0.00 s" is a time too short for its
// rounding, not none.
func roundTook(took string, secs float64) string {
	switch {
	case took == "":
		return ""
	case secs == 0:
		_, unit, _ := strings.Cut(took, " ")
		return map[string]string{"s": "in under 0.01 s", "ms": "in under 1 ms"}[unit]
	}
	return "in " + took
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
