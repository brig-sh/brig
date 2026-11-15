// Package hullstub turns a test's stub script into a stub of the macOS hull.
//
// It is a test helper like internal/ttytest: nothing outside a test uses it.
// On a Linux host brig drives a hull as the macOS one only when its --version
// says it was built for darwin, and a stub that answers nothing is the Linux
// hull. Tests that check the macOS paths run on Linux CI, so their stubs say
// darwin, and they say it here once: a copy per package would be a copy to
// miss when hull's --version moves. script/smoke.sh's stub, in bash, keeps
// its own.
package hullstub

import "strings"

// MacOS is script with a --version answer of the macOS hull inserted after
// its shebang line. The version carries no release number, so what brig reads
// from it is what it read from a stub that answered nothing.
func MacOS(script string) string {
	shebang, rest, _ := strings.Cut(script, "\n")
	return shebang + "\n[ \"$1\" = --version ] && { echo 'hull dev (go1.26.4, darwin/arm64)'; exit 0; }\n" + rest
}
