package main

import (
	"fmt"
	"strings"

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
	return fmt.Sprintf("`%s` is one word, so the guest looks for a command by that name. "+
		"To run it as a script, put -c in front of it", word)
}
