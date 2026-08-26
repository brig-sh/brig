package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// TelemetryState is a runtime's telemetry setting, in brig's own words.
//
// The words are brig's rather than the runtime's on purpose. hull phrases its
// state for someone who installed hull, and a brig user has not necessarily
// heard of it: the point of `brig telemetry` is that opting out does not
// require knowing which binary underneath does the sending. The mapping from
// what a runtime says to one of these lives in this file, so the CLI above
// never parses another tool's sentences.
type TelemetryState string

const (
	// TelemetryOn means someone answered yes and events go out.
	TelemetryOn TelemetryState = "on"
	// TelemetryOff means events do not go out, whether that is a recorded no
	// or a variable in this shell.
	TelemetryOff TelemetryState = "off"
	// TelemetryUnanswered means no answer has been recorded that covers what
	// would be collected today. Nothing has been agreed to, so brig treats a
	// boot as not countable; see telemetryEnvFor.
	TelemetryUnanswered TelemetryState = "unanswered"
	// TelemetryUnknown means the runtime did not say. An older build with no
	// telemetry command at all lands here, and so does a build that answered
	// something this version has never seen.
	TelemetryUnknown TelemetryState = "unknown"
)

// Telemetry is what a runtime reports about its own collection.
type Telemetry struct {
	State TelemetryState
	// Setting names the environment variable that decided the state, as
	// NAME=VALUE, when a variable in this shell decided it rather than a
	// recorded answer. Empty otherwise. Worth carrying because "off" with no
	// explanation is the state people then cannot turn back on: the answer is
	// in their shell, not on their disk, and only naming the variable says so.
	Setting string
}

// TelemetryReporter is a runtime whose collection brig can report and change.
//
// Optional on purpose: it is not part of Runtime because it is not part of
// running a sandbox, and a backend that collects nothing should not have to
// grow two stub methods to say so. The CLI type-asserts for it and treats a
// runtime that does not implement it as one that sends nothing, which is what
// nerdctl is.
type TelemetryReporter interface {
	// TelemetryStatus reports the effective state, including a variable in
	// the environment that overrides whatever is on disk.
	TelemetryStatus() (Telemetry, error)
	// SetTelemetry records an answer durably, for every later invocation.
	SetTelemetry(on bool) error
}

// telemetryCallTimeout bounds the extra invocation a boot pays to find out
// whether it may be counted.
//
// It reads a small file in the runtime's own store, so it is fast or it is
// broken. Bounding it keeps a wedged runtime binary from turning a question
// about telemetry into a `brig run` that never boots: the answer this call
// exists to fetch decides one environment variable, and having no answer is
// already handled -- it suppresses.
const telemetryCallTimeout = 3 * time.Second

// TelemetryStatus asks hull what its effective state is.
func (h *hull) TelemetryStatus() (Telemetry, error) {
	out, err := h.telemetryCmd("status")
	if err != nil {
		return Telemetry{State: TelemetryUnknown}, err
	}
	return parseTelemetry(out), nil
}

// SetTelemetry records the answer where hull keeps it.
//
// Deliberately not a store of brig's own. Two files answering the same
// question drift, and the one that would lose is brig's: hull is what actually
// decides whether to send, so an answer it cannot see is not an answer at all.
// This is also why `brig telemetry off` is durable rather than a variable brig
// sets per invocation -- an opt-out that only holds while brig is the caller
// is not the opt-out anyone means.
func (h *hull) SetTelemetry(on bool) error {
	verb := "off"
	if on {
		verb = "on"
	}
	_, err := h.telemetryCmd(verb)
	return err
}

// telemetryCmd runs one hull telemetry verb and returns its stdout.
//
// Suppressed like every other question brig asks on its own account: asking
// what the setting is, or changing it, must not itself become an event.
func (h *hull) telemetryCmd(verb string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), telemetryCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, "telemetry", verb)
	cmd.WaitDelay = time.Second
	cmd.Env = mergeEnv(telemetryEnv(false))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if said := strings.TrimSpace(errb.String()); said != "" {
			return "", fmt.Errorf("%s telemetry %s: %w: %s", h.bin, verb, err, firstLines(said, 2))
		}
		return "", fmt.Errorf("%s telemetry %s: %w", h.bin, verb, err)
	}
	return out.String(), nil
}

// parseTelemetry turns hull's one line into a state brig can report.
//
// hull prints "telemetry: <state>", where the state is one of a handful of
// phrases documented in its docs/telemetry.md. Anything else is read as
// unknown rather than guessed at: a phrase this version has not seen is a
// version that collects something this version cannot describe, and reporting
// that as "on" would put brig's name on a claim it cannot support.
func parseTelemetry(out string) Telemetry {
	state := strings.TrimSpace(out)
	state = strings.TrimSpace(strings.TrimPrefix(state, "telemetry:"))
	switch {
	case state == "enabled":
		return Telemetry{State: TelemetryOn}
	case state == "disabled":
		return Telemetry{State: TelemetryOff}
	case strings.HasPrefix(state, "disabled ("):
		return Telemetry{State: TelemetryOff, Setting: insideParens(state)}
	case strings.HasPrefix(state, "enabled ("):
		// The mirror of "disabled (...)": the parens name the setting that
		// turned it on (a variable, say), and the state is definite. Read it as
		// on and carry the setting, rather than folding it into the older-schema
		// case below and reporting a definite answer as unanswered.
		return Telemetry{State: TelemetryOn, Setting: insideParens(state)}
	case strings.HasPrefix(state, "not configured"):
		return Telemetry{State: TelemetryUnanswered}
	case strings.HasPrefix(state, "enabled "):
		// "enabled for an older schema": there is an answer on disk, but it
		// covers a smaller list of fields than this build would send, so hull
		// will ask again. An answer to a question that has since changed is
		// not an answer to this one, which is exactly the case brig must not
		// count a boot for.
		return Telemetry{State: TelemetryUnanswered}
	default:
		return Telemetry{State: TelemetryUnknown}
	}
}

// insideParens lifts the "DO_NOT_TRACK=1" out of "disabled (DO_NOT_TRACK=1)".
func insideParens(s string) string {
	open := strings.Index(s, "(")
	closing := strings.LastIndex(s, ")")
	if open < 0 || closing < open {
		return ""
	}
	return strings.TrimSpace(s[open+1 : closing])
}

// consentRecorded reports whether an answer covering today's collection is on
// file, and caches it for the rest of the process.
//
// One brig command boots at most one sandbox, so the cache saves a second
// invocation rather than many; it is here because the answer cannot change
// underneath a single command in any way brig would want to act on. A runtime
// that will not answer counts as no answer, which suppresses.
func (h *hull) consentRecorded() bool {
	h.consent.once.Do(func() {
		st, err := h.TelemetryStatus()
		h.consent.on = err == nil && st.State == TelemetryOn
	})
	return h.consent.on
}

// telemetryEnvFor decides whether an operation the user asked for is actually
// counted, on top of telemetryEnv's plumbing rule.
//
// hull defaults telemetry on when it cannot prompt, and brig's boot invocation
// gives it no terminal: left alone, a fresh install sends events for its first
// boot before anyone has been asked anything, and the prompt then appears
// later on an interactive step wearing brig's name. So brig suppresses the
// boot until hull reports a recorded answer. The alternative -- brig asking
// the question itself, on its own terminal, before the first boot -- would put
// the disclosure in brig's hands, but it also means brig owning a consent
// prompt whose wording has to track what hull collects, and it cannot run at
// all for `brig run` from a script.
//
// canAsk is for the one invocation that hands hull the terminal. There the
// question can be put to a human before anything is sent, which is hull's own
// design, so suppressing would only prevent the prompt from ever appearing --
// and with it any chance of an answer.
func (h *hull) telemetryEnvFor(counted, canAsk bool) []string {
	return telemetryEnv(counted && (canAsk || h.consentRecorded()))
}
