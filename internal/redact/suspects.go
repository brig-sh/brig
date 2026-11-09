package redact

import (
	"math"
	"regexp"
	"strings"
)

// Suspect is a string left in masked text that looks like a credential of a
// shape the rules do not know.
type Suspect struct {
	// Line is the 1-based line it is on.
	Line int
	// Start is its first four characters, enough to find it and not
	// enough to use it.
	Start string
	Len   int
}

// suspectRun is a run of the characters base64, base64url and most token
// formats use. A path separator or a period ends it, so a path is read one
// segment at a time.
var suspectRun = regexp.MustCompile(`[A-Za-z0-9_+=-]{24,}`)

// minEntropy is the Shannon entropy, in bits per character, below which a
// run reads as a word or a name. A random base64 run of 24 characters is
// near 4.3; an identifier such as TestRedactMasksPlantedValues is near 3.8.
const minEntropy = 4.0

// Suspects returns the strings in s that look random: 24 characters or more
// of upper case, lower case and digits mixed together, not hex, and of high
// entropy. It masks nothing. The rules mask by shape and Brig does not read
// the values it holds, so this is the check that can point at a credential
// of an unknown shape: the caller shows where they are for the reader to
// look at before the text is shared. Hashes, UUIDs and random lower-case
// directory names do not count.
func Suspects(s string) []Suspect {
	var out []Suspect
	for i, line := range strings.Split(s, "\n") {
		for _, run := range suspectRun.FindAllString(line, -1) {
			if looksRandom(run) {
				out = append(out, Suspect{Line: i + 1, Start: run[:4], Len: len(run)})
			}
		}
	}
	return out
}

// minMixed is how many times a suspect must switch between a digit and a
// letter. Random text does so throughout; a name with a number after it,
// such as TestReportFileIsPrivate134726287, does once.
const minMixed = 3

func looksRandom(s string) bool {
	var upper, lower, nonHex bool
	mixed := 0
	counts := map[rune]int{}
	prevDigit, first := false, true
	for _, c := range s {
		counts[c]++
		isDigit := c >= '0' && c <= '9'
		isLetter := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
		switch {
		case c >= 'A' && c <= 'Z':
			upper = true
			nonHex = nonHex || c > 'F'
		case c >= 'a' && c <= 'z':
			lower = true
			nonHex = nonHex || c > 'f'
		}
		if isDigit || isLetter {
			if !first && isDigit != prevDigit {
				mixed++
			}
			prevDigit, first = isDigit, false
		}
	}
	if !upper || !lower || !nonHex || mixed < minMixed {
		return false
	}
	var h float64
	for _, n := range counts {
		p := float64(n) / float64(len(s))
		h -= p * math.Log2(p)
	}
	return h >= minEntropy
}
