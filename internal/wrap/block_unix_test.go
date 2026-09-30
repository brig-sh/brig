//go:build darwin || linux

package wrap

import (
	"bufio"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/ttytest"
)

// On a terminal a block is one heading with its rows under it: the prefix
// goes on the heading alone, and a blank line parts it from the block before.
func TestWarnBlockOnATerminalPrefixesOnlyTheHeading(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	ptm, tty := ttytest.Pair(t)
	c := &Config{Err: tty, Verbosity: Normal}
	c.warnf("%s", "first heading\n  ○ a row")
	c.warnf("%s", "second heading\n  → a command")

	want := []string{"brig: first heading", "  ○ a row", "", "brig: second heading", "  → a command"}
	lines := make(chan string)
	go func() {
		r := bufio.NewReader(ptm)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			lines <- strings.TrimRight(line, "\r\n")
		}
	}()
	for i, w := range want {
		select {
		case got := <-lines:
			if got != w {
				t.Errorf("line %d = %q, want %q", i, got, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("line %d never arrived, want %q", i, w)
		}
	}
}

// A one-line warning after a block gets a blank line too, or it reads as one
// more row of the block. One-line warnings in a row stay together.
func TestWarnfAfterABlockIsPartedFromIt(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	ptm, tty := ttytest.Pair(t)
	c := &Config{Err: tty, Verbosity: Normal}
	c.warnf("one line")
	c.warnf("another line")
	c.warnf("%s", "a heading\n  ○ a row")
	c.warnf("after the block")

	want := []string{"brig: one line", "brig: another line", "", "brig: a heading",
		"  ○ a row", "", "brig: after the block"}
	r := bufio.NewReader(ptm)
	lines := make(chan string)
	go func() {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			lines <- strings.TrimRight(line, "\r\n")
		}
	}()
	for i, w := range want {
		select {
		case got := <-lines:
			if got != w {
				t.Errorf("line %d = %q, want %q", i, got, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("line %d never arrived, want %q", i, w)
		}
	}
}
