package wrap

import (
	"fmt"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/runtime"
)

// answerEnd is the line the guest prints after the answer to one of brig's
// questions. Output that does not end with it is no answer.
const answerEnd = "brig-answer-end"

// answerScript runs the question, then prints answerEnd on a line of its own.
// A question that fails exits with its own status and prints no answerEnd.
const answerScript = `"$@" || exit; printf '\n%s\n' ` + answerEnd

// answerScriptHeld is answerScript with the process kept up for 0.1s after
// the answer. A runtime that loses the output of a process that exits at once
// keeps it then, so every try after the first uses it. A sleep that fails, or
// takes no fraction, costs only the wait.
const answerScriptHeld = answerScript + `; sleep 0.1 2>/dev/null || :`

// answerTries is how many times one question is asked before brig gives up.
const answerTries = 12

// answerPause is the wait before the second try. Each wait after it doubles,
// up to answerPauseMax, so the tries span several seconds and outlast a burst
// of lost output.
const (
	answerPause    = 100 * time.Millisecond
	answerPauseMax = time.Second
)

// answerSleep waits between two tries. It is a variable so tests can skip the
// wait.
var answerSleep = time.Sleep

// ask runs a read-only command in the guest and returns what it printed.
//
// A runtime can report an exec as successful and return none of its output,
// or only the start of it. Taken as the answer, an empty mount table reads as
// nothing mounted, and an empty filesystem type as a directory on host disk.
// So the command runs under answerScript, and output that does not end with
// answerEnd is asked for again under answerScriptHeld. A question that never
// gets a whole answer fails and says so.
//
// Asking again is safe because every question is a read. A command that
// changes the guest goes through guestRoot, which reads only the exit status.
func (c *Config) ask(spec runtime.ExecSpec) (string, error) {
	argv := spec.Cmd
	script := answerScript
	pause := answerPause
	for try := 1; ; try++ {
		spec.Cmd = append([]string{"sh", "-c", script, "sh"}, argv...)
		out, err := c.Runtime.Output(spec)
		if err != nil {
			return "", err
		}
		if answer, ok := strings.CutSuffix(out, "\n"+answerEnd+"\n"); ok {
			return answer, nil
		}
		if try == answerTries {
			return "", fmt.Errorf("the sandbox gave no answer to `%s` in %d tries: the "+
				"runtime ran it each time and returned no complete output",
				strings.Join(argv, " "), answerTries)
		}
		answerSleep(pause)
		pause = min(2*pause, answerPauseMax)
		script = answerScriptHeld
	}
}
