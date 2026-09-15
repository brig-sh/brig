package main

import (
	"strings"
	"testing"
)

// --publish is brig's, repeatable, and it reaches Load as written.
func TestPublishFlagCollects(t *testing.T) {
	o, _, tail, err := parse("run", []string{"--publish", "3000", "claude", "--publish", "8080:80"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := strings.Join(o.load.Publish, " "); got != "3000 8080:80" {
		t.Fatalf("publish = %q, want both, in order", got)
	}
	if len(tail) != 0 {
		t.Fatalf("tail = %v, want nothing left for the agent", tail)
	}
}

// -p is NOT brig's. It is claude's print flag and docker's publish flag, and
// brig owns only one of those readings -- so `brig run claude -p "fix the
// tests"` has to stay a prompt. TestBrigFlagsOverlapWithShippedAgentsOnlyWhereKnown
// guards the table; this guards the line.
func TestShortPStaysTheAgents(t *testing.T) {
	o, _, tail, err := parse("run", []string{"claude", "-p", "fix the tests"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(o.load.Publish) != 0 {
		t.Fatalf("brig read -p as a published port: %v", o.load.Publish)
	}
	if strings.Join(tail, " ") != "-p fix the tests" {
		t.Fatalf("tail = %v, want the agent's own flag untouched", tail)
	}
}

// publish and unpublish take their ports as bare words, which is the position
// an agent's argv would otherwise occupy -- and they have no agent.
func TestPublishVerbsTakeBareWordsAsPorts(t *testing.T) {
	for _, verb := range []string{"publish", "unpublish"} {
		_, profileName, tail, err := parse(verb, []string{"claude@web", "8080:80", "3000"})
		if err != nil {
			t.Fatalf("parse %s: %v", verb, err)
		}
		if profileName != "claude" {
			t.Errorf("%s: profile = %q", verb, profileName)
		}
		if strings.Join(tail, " ") != "8080:80 3000" {
			t.Errorf("%s: ports = %v", verb, tail)
		}
	}
}

// --all is legal on the publish line and nowhere else; a run-line flag is not
// legal there at all, because those verbs shape no run.
func TestPublishLineHasAVocabularyOfItsOwn(t *testing.T) {
	o, _, tail, err := parse("unpublish", []string{"claude@web", "--all"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !o.all {
		t.Error("--all was not read")
	}
	if len(tail) != 0 {
		t.Errorf("tail = %v", tail)
	}
	// A run-line flag here is a mistake to name rather than a port to fail on
	// with a message about port numbers.
	_, _, _, err = parse("publish", []string{"claude@web", "--mem", "4096"})
	if err == nil {
		t.Fatal("--mem on the publish line was accepted")
	}
	if !strings.Contains(err.Error(), "--mem") || !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("the refusal does not name the flag: %v", err)
	}
}

// Every verb that takes ports keeps its tail, and rejectTail must then not
// refuse it. The two predicates read the same set for exactly this reason.
func TestPortVerbsKeepTheirOperands(t *testing.T) {
	for _, verb := range []string{"publish", "unpublish"} {
		if !takesPorts(verb) {
			t.Fatalf("%s does not take ports", verb)
		}
		if err := rejectTail(verb, []string{"8080"}); err != nil {
			t.Errorf("%s refused its own port: %v", verb, err)
		}
		if forwardsTail(verb) {
			t.Errorf("%s forwards a tail to an agent it does not have", verb)
		}
	}
	// And a verb that takes neither still refuses a stray word.
	if err := rejectTail("stop", []string{"8080"}); err == nil {
		t.Error("stop accepted a trailing word")
	}
}
