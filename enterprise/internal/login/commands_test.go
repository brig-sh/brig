// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package login

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// The command line asks each command what to offer. brig login offers its
// flags, and neither the value of --ttl nor the URL, which are typed. brig
// logout takes no flag, so it offers nothing.
func TestLoginCompletesItsFlags(t *testing.T) {
	byVerb := map[string]func([]string, string) []string{}
	for _, c := range Commands() {
		byVerb[c.Verb] = c.Complete
	}
	if got := byVerb["login"](nil, "-"); !slices.Equal(got, []string{"--browser", "--device", "--ttl"}) {
		t.Errorf("login - offers %v", got)
	}
	if got := byVerb["login"]([]string{"--ttl"}, ""); got != nil {
		t.Errorf("the value of --ttl offers %v, want nothing", got)
	}
	if got := byVerb["login"](nil, ""); got != nil {
		t.Errorf("the URL offers %v, want nothing", got)
	}
	if byVerb["logout"] != nil {
		t.Error("logout offers words, and it takes no flag")
	}
}

// Asking a verb for help is answered with its usage, on out, and no error.
// The usage brig help prints is the same text, and the help list names the
// verb in its first line.
func TestLoginAndLogoutAnswerHelp(t *testing.T) {
	for _, c := range Commands() {
		for _, flag := range []string{"--help", "-h"} {
			var out bytes.Buffer
			if err := c.Run(&out, []string{flag}); err != nil {
				t.Errorf("brig %s %s: %v", c.Verb, flag, err)
			}
			if out.String() != c.Usage || !strings.HasPrefix(c.Usage, "brig "+c.Verb+" -- ") {
				t.Errorf("brig %s %s printed %q, want its usage", c.Verb, flag, out.String())
			}
		}
		if !strings.HasPrefix(c.Synopsis, "  brig "+c.Verb+" ") || !strings.HasSuffix(c.Synopsis, "\n") {
			t.Errorf("the help line of brig %s is %q", c.Verb, c.Synopsis)
		}
	}
}
