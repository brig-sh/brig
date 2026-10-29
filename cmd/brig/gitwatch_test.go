package main

import (
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// listedRuntime is jsonRuntime that also lists the one sandbox the cases below
// act on, which rm asks for before it removes anything.
type listedRuntime struct{ jsonRuntime }

func (r *listedRuntime) Isolation(runtime.RunSpec) runtime.Isolation { return runtime.Isolation{} }

func (r *listedRuntime) List() ([]runtime.Instance, error) {
	return []runtime.Instance{{Name: "brig-faker", State: "running"}}, nil
}

// checksProjectGit is the one place that says which verbs look at the project's
// .git: the ones that start, enter, stop or remove a session's sandbox, and no
// preview or report. Every verb that reaches it is listed, so one added to the
// run line has to be placed here, and the answer for each is stated and not
// derived from the function under test.
func TestChecksProjectGitIsExactlyTheVerbsThatActOnASandbox(t *testing.T) {
	for verb, want := range map[string]bool{
		"run": true, "create": true, "sh": true, "exec": true, "stop": true, "rm": true,
		"info": false, "env": false, "plan": false,
		"network ls": false, "network publish": false, "network unpublish": false,
		"logs": false, "": false,
	} {
		if got := checksProjectGit(verb); got != want {
			t.Errorf("checksProjectGit(%q) = %v, want %v", verb, got, want)
		}
	}
}

// The table above says what the function answers, and nothing in it says the
// call is made. Each verb is run through the whole of run() with a planted
// hook behind a look the sandbox already has: the ones that act on a sandbox
// name it, and info and plan, which only report, do not, and do not move the
// baseline either, so the verb after them still can.
func TestEachVerbLooksAtTheProjectsGitOrDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		looks bool
	}{
		{"run", []string{"run", "faker", "-d"}, true},
		{"create", []string{"create", "faker"}, true},
		{"sh", []string{"sh", "faker", "true"}, true},
		{"exec", []string{"exec", "faker", "true"}, true},
		{"stop", []string{"stop", "faker"}, true},
		{"rm", []string{"rm", "faker"}, true},
		{"info", []string{"info", "faker"}, false},
		{"plan", []string{"plan", "faker"}, false},
		{"env", []string{"env", "faker"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			removeHost(t, &listedRuntime{})
			plantHook(t, "brig-faker")
			stderr := captureStderr(t, func() { _, _ = run2(t, tc.args) })
			if got := strings.Contains(stderr, "added .git/hooks/post-checkout"); got != tc.looks {
				t.Errorf("brig %s: warned %v, want %v:\n%s", strings.Join(tc.args, " "), got, tc.looks, stderr)
			}
		})
	}
}
