package main

import (
	"strings"

	"github.com/brig-sh/brig/internal/notice"
	"github.com/brig-sh/brig/internal/wrap"
)

// checkShellCommand reads a guest command bound for the login shell before
// anything boots. A script flag with no script after it is refused here, as a
// usage error, rather than once the sandbox is up. Shell makes the same check
// for any caller that does not come through here. A command that looks like a
// script typed as one word gets a hint and still runs.
func checkShellCommand(tail []string) error {
	if err := wrap.ShellCommandError(tail); err != nil {
		return usagef("%s", err)
	}
	if hint := shellHint(tail); hint != "" {
		warnf("%s", hint)
	}
	return nil
}

// shellHint names -c when the command is one word that reads as a script:
// it holds a shell operator, a redirection, a $ or a backquote, or a space
// and does not start with a /. That is a script typed the way ssh takes one,
// and the guest looks it up as a command name, which fails with a message
// about a missing file when the word has a slash in it. A word that starts
// with / and has no operator is a program path with a space in it, which
// is a real command, and -c would split it.
func shellHint(tail []string) string {
	if len(tail) != 1 {
		return ""
	}
	word := tail[0]
	script := strings.ContainsAny(word, "|;&<>$`") ||
		(strings.ContainsAny(word, " \t\n") && !strings.HasPrefix(word, "/"))
	if !script {
		return ""
	}
	return notice.Newf("`%s` is one word, so the guest looks for a command by that name", hintHeading(word)).
		Do("to run it as a script, put -c in front of it", "").String()
}

// hintWidth is how much of the word the hint's heading quotes, in runes.
const hintWidth = 40

// hintHeading returns the word as the hint's heading quotes it: its first
// non-blank line, cut to hintWidth runes, with " …" when anything is left
// out. The heading is one line of a notice block. Off a terminal each line
// of the block gets brig's prefix, so a script typed over several lines
// would split the heading.
func hintHeading(word string) string {
	line := strings.TrimLeft(word, " \t\r\n")
	more := false
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line, more = line[:i], true
	}
	if r := []rune(line); len(r) > hintWidth {
		line, more = string(r[:hintWidth]), true
	}
	if more {
		line = strings.TrimRight(line, " \t") + " …"
	}
	return line
}
