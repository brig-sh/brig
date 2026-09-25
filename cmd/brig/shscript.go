package main

import (
	"fmt"
	"strings"
)

// checkShellCommand reads a guest command bound for the login shell before
// anything boots. A -c with no script after it is refused here, where the
// message can say what -c wants, rather than left for bash to report once the
// sandbox is up. A command that looks like a script typed as one word gets a
// hint and still runs.
func checkShellCommand(tail []string) error {
	if len(tail) == 1 && tail[0] == "-c" {
		return usagef("-c needs a script after it, for example -c 'ls /work | wc -l'")
	}
	if hint := shellHint(tail); hint != "" {
		warnf("%s", hint)
	}
	return nil
}

// shellHint names -c when the command is one word with a space or a shell
// operator in it. That is a script typed the way ssh takes one, and the guest
// looks it up as a command name, which fails with a message about a missing
// file when the word has a slash in it. A one-word command with a space in it
// is almost never a real program, so the hint is close to never wrong, and
// the command runs either way.
func shellHint(tail []string) string {
	if len(tail) != 1 || !strings.ContainsAny(tail[0], " \t\n|;&") {
		return ""
	}
	return fmt.Sprintf("`%s` is one word, so the guest looks for a command by that name. "+
		"To run it as a script, put -c in front of it", tail[0])
}
