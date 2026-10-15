package runtime

import (
	"regexp"
	"strings"
)

// hull prints twenty to thirty lines for one move: each round, hvi's figures
// for the pause, what it sent and how the destination checked it, the
// timings of each step and the seal's timeline on both hosts. All of it is
// in the move's report, and in --json. A person watching the move needs the
// shape of it: the rounds, the pause, the seal, the restore and the resume.
// QuietLine picks those out, each in a few words, and brig prints every
// line whole only under --verbose.

// The lines QuietLine reads besides the rounds, which lineRound reads. Each
// is hull 7b3f7b1's, the streamed move first and then the whole-bundle one:
//
//	move: hvi: 2 RAM rounds (converged), disk rounds of 3876, 0 blocks (...); paused 24 ms, ...
//	move: hvi sealed the stream and fenced brig-ubuntu (19 ms after the pause)
//	Sent 44.4 MiB in 1.2 s (36.7 MiB/s): disk ...; destination: stream: ... and restored the guest paused
//	Moved brig-ubuntu to pmoust@rp2: it runs there at epoch 1. Its checkpoint here is stale
//	Fencing brig-ubuntu (checkpoint with then=fence)
//	Fenced brig-ubuntu: checkpoint /path
//	Restoring brig-ubuntu paused on pmoust@rp2
//
// and two that say why the pause is long, or a wait before it: the base sent
// first to a host that did not hold it, and a destination that copied the
// base while the guest was paused.
var (
	quietPause      = regexp.MustCompile(`^(?:move: )?hvi: (\d+ RAM rounds?(?: \([^)]*\))?)`)
	quietSealed     = regexp.MustCompile(`^(?:move: )?hvi sealed the stream\b[^(]*(?:\((\d+ ms after the [^)]*)\))?`)
	quietSent       = regexp.MustCompile(`^Sent (\d+(?:\.\d+)? (?:bytes|KiB|MiB|GiB)) in (\d+(?:\.\d+)? s)\b(.*)$`)
	quietMoved      = regexp.MustCompile(`^Moved \S+ to (\S+): it runs there at epoch (\d+)`)
	quietFencing    = regexp.MustCompile(`^Fencing (\S+)`)
	quietFenced     = regexp.MustCompile(`^Fenced (\S+):`)
	quietRestoring  = regexp.MustCompile(`^Restoring (\S+) paused on (\S+)`)
	quietSendBase   = regexp.MustCompile(`^(Sending base \S+ to \S+)`)
	quietTookStream = regexp.MustCompile(`^move: (the destination took the stream in .*)$`)
)

// QuietLine is what brig prints by default of one line hull printed during a
// move, and whether it prints anything: a round as its name, size and time
// ("ram-round-0 40.6 MiB in 1.00 s"), the pause, the seal, the restore and
// the resume each in a few words, and a warning or an error as hull wrote
// it. Every other line is for --verbose. A round's progress while it runs
// ("so far") is not a round.
func QuietLine(line string) (string, bool) {
	line = strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(line, "warning:"), strings.HasPrefix(line, "error:"):
		return line, true
	}
	if m := lineRound.FindStringSubmatch(line); m != nil {
		rest := m[4]
		if strings.Contains(rest, " so far") {
			return "", false
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(line, "move: "), ":")
		said := []string{name}
		if sm := lineSize.FindStringSubmatch(rest); sm != nil {
			said = append(said, sm[1])
		}
		if tm := lineSeconds.FindStringSubmatch(rest); tm != nil {
			said = append(said, "in "+tm[1]+" "+tm[2])
		}
		return strings.Join(said, " "), true
	}
	if m := quietPause.FindStringSubmatch(line); m != nil {
		return "paused after " + m[1], true
	}
	if m := quietSealed.FindStringSubmatch(line); m != nil {
		if m[1] != "" {
			return "sealed " + m[1], true
		}
		return "sealed", true
	}
	if m := quietSent.FindStringSubmatch(line); m != nil {
		said := "sent " + m[1] + " in " + m[2]
		if _, dest, ok := strings.Cut(m[3], "destination: "); ok && strings.Contains(dest, "restored the guest paused") {
			said += "; restored paused there"
		}
		return said, true
	}
	if m := quietMoved.FindStringSubmatch(line); m != nil {
		return "resumed on " + m[1] + " (epoch " + m[2] + ")", true
	}
	if m := quietFencing.FindStringSubmatch(line); m != nil {
		return "paused to fence " + m[1], true
	}
	if m := quietFenced.FindStringSubmatch(line); m != nil {
		return "fenced " + m[1], true
	}
	if m := quietRestoring.FindStringSubmatch(line); m != nil {
		return "restoring " + m[1] + " paused on " + m[2], true
	}
	if m := quietSendBase.FindStringSubmatch(line); m != nil {
		return strings.ToLower(m[1][:1]) + m[1][1:] + " first", true
	}
	if m := quietTookStream.FindStringSubmatch(line); m != nil {
		return m[1], true
	}
	return "", false
}
