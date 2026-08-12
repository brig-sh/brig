package main

import (
	"strings"
	"testing"
)

// The parse cases the bash wrapper's own suite ran, plus the ones the verb
// form and the sandbox flags add. brig's flags are read off the front in any
// order and everything from the first unrecognised argument onwards belongs
// to the agent.
func TestParse(t *testing.T) {
	cases := []struct {
		args     []string
		name     string
		given    bool
		template string
		tail     string
	}{
		{[]string{"claude"}, "", false, "claude", ""},
		{[]string{"claude", "--name", "foo"}, "foo", true, "claude", ""},
		{[]string{"claude", "-n", "foo"}, "foo", true, "claude", ""},
		{[]string{"claude", "--name=foo"}, "foo", true, "claude", ""},
		{[]string{"--name", "foo", "claude"}, "foo", true, "claude", ""},
		// Everything from the first agent argument onwards passes through.
		{[]string{"claude", "-p", "hi"}, "", false, "claude", "-p hi"},
		{[]string{"claude", "-n", "foo", "-p", "hi"}, "foo", true, "claude", "-p hi"},
		// A -- ends brig's own parsing, so an agent argument spelled like one
		// of brig's flags still reaches the agent.
		{[]string{"claude", "--", "--name", "notasession"}, "", false, "claude", "--name notasession"},
		{[]string{}, "", false, "", ""},
		{[]string{"claude", "echo", "hi", "there"}, "", false, "claude", "echo hi there"},
	}
	for _, c := range cases {
		o, template, tail, err := parse(c.args)
		if err != nil {
			t.Errorf("parse(%q) failed: %v", c.args, err)
			continue
		}
		if o.load.Name != c.name || o.nameGiven != c.given || template != c.template ||
			strings.Join(tail, " ") != c.tail {
			t.Errorf("parse(%q) = (%q, %v, %q, %q), want (%q, %v, %q, %q)",
				c.args, o.load.Name, o.nameGiven, template, strings.Join(tail, " "),
				c.name, c.given, c.template, c.tail)
		}
	}
}

func TestParseSandboxFlags(t *testing.T) {
	o, template, tail, err := parse([]string{
		"claude", "-t", "ghcr.io/me/img:latest", "-w", "/tmp/ws",
		"-m", "8192", "--cpus", "2", "-d", "-p", "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if template != "claude" {
		t.Errorf("template = %q", template)
	}
	if o.load.Image != "ghcr.io/me/img:latest" || o.load.Workspace != "/tmp/ws" {
		t.Errorf("image/workspace = %q, %q", o.load.Image, o.load.Workspace)
	}
	if o.load.Mem != 8192 || o.load.CPUs != 2 {
		t.Errorf("mem/cpus = %d, %d", o.load.Mem, o.load.CPUs)
	}
	if !o.detach {
		t.Error("-d did not set detach")
	}
	// The agent's own arguments still pass through untouched.
	if strings.Join(tail, " ") != "-p hi" {
		t.Errorf("tail = %q", tail)
	}
	// The inline form works for every value flag.
	o, _, _, err = parse([]string{"claude", "--image=x", "--workspace=/w", "--memory=1024", "--cpus=1"})
	if err != nil {
		t.Fatal(err)
	}
	if o.load.Image != "x" || o.load.Workspace != "/w" || o.load.Mem != 1024 || o.load.CPUs != 1 {
		t.Errorf("inline form = %+v", o.load)
	}
}

func TestParseRejectsBadValues(t *testing.T) {
	cases := [][]string{
		{"claude", "--name"},      // nothing after it
		{"claude", "--name", ""},  // empty reads like no flag at all
		{"claude", "-m", "lots"},  // not a number
		{"claude", "--cpus", "0"}, // not a useful one
		{"claude", "-m", "-4"},    //
		{"claude", "--image"},     // value flag with nothing after it
	}
	for _, args := range cases {
		if _, _, _, err := parse(args); err == nil {
			t.Errorf("parse(%q) was accepted", args)
		}
	}
}
