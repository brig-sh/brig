// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package nofireauth

import (
	"strings"
	"testing"
)

func TestParseDeploymentAccepted(t *testing.T) {
	for in, want := range map[string]string{
		"https://localhost:8443":       "https://localhost:8443",
		"https://h/A-b_c.d~e/":         "https://h/A-b_c.d~e",
		"https://my_host.example":      "https://my_host.example",
		"https://a-b.example":          "https://a-b.example",
		"https://h.example1":           "https://h.example1",
		"my.nofire.ai":                 "https://my.nofire.ai",
		"https://MY.nofire.ai/":        "https://my.nofire.ai",
		"https://my.nofire.ai:443":     "https://my.nofire.ai",
		"nofire.acme.internal:8443":    "https://nofire.acme.internal:8443",
		"https://h/prefix/":            "https://h/prefix",
		"http://127.0.0.1:3000":        "http://127.0.0.1:3000",
		"http://[::1]:3000":            "http://[::1]:3000",
		"https://[2001:DB8::1]:443/x/": "https://[2001:db8::1]/x",
	} {
		d, err := ParseDeployment(in)
		if err != nil {
			t.Errorf("ParseDeployment(%q): %v", in, err)
			continue
		}
		if got := d.String(); got != want {
			t.Errorf("ParseDeployment(%q) = %q, want %q", in, got, want)
		}
		again, err := ParseDeployment(d.String())
		if err != nil || again.String() != d.String() {
			t.Errorf("not idempotent for %q: %v, %v", in, again, err)
		}
	}
}

func TestParseDeploymentRefused(t *testing.T) {
	seen := map[string]string{}
	for in, why := range map[string]string{
		"https://my.nofire.ai@evil.example": "user",
		"https://h/?q=1":                    "query",
		"https://h/#f":                      "fragment",
		"https://nöfire.ai":                 "non-ASCII",
		"https://xn--nfire-jua.ai":          "punycode",
		"ftp://h":                           "scheme",
		"http://h":                          "http",
		"http://localhost:3000":             "localhost",
		"":                                  "empty",
		"https://h/a/../b":                  "dot segment",
		"https://h:0":                       "port",
		"https://h/a//":                     "empty segment",
		"https://h//x":                      "empty segment 2",
		"https://h/%2e%2e/x":                "encoded dot",
		"https://h/a/%2E":                   "encoded dot 2",
		"https://[fe80::1%25eth0]":          "zone",
		"https://a b.example":               "bad char",
		"https://a$b.example":               "bad char 2",
		"https://h:99999":                   "port",
		"https://h/a%2Fb":                   "encoded slash",
		"https://h/a%5Cb":                   "encoded backslash",
		"https://h/%41":                     "encoded letter",
		"https://h/a%20b":                   "encoded space",
		"https://h/;@evil.example":          "semicolon and at",
		"https://h/a@b":                     "at",
		"https://2130706433":                "decimal number host",
		"https://0x7f.1":                    "hex number host",
		"https://0177.0.0.1":                "octal address",
		"https://h.0x7f":                    "hex last label",
		"https://-a.example":                "leading hyphen",
		"https://a-.example":                "trailing hyphen",
	} {
		_, err := ParseDeployment(in)
		if err == nil {
			t.Errorf("ParseDeployment(%q) accepted, want refusal (%s)", in, why)
			continue
		}
		if prev, dup := seen[err.Error()]; dup {
			t.Errorf("%q and %q share the message %q", in, prev, err)
		}
		seen[err.Error()] = in
	}
}

func TestResolveDeployment(t *testing.T) {
	d, src, err := ResolveDeployment("https://arg.example", "env.example")
	if err != nil || src != FromArg || d.String() != "https://arg.example" {
		t.Fatalf("arg: %v %v %v", d, src, err)
	}
	d, src, err = ResolveDeployment("", "env.example")
	if err != nil || src != FromEnv || d.String() != "https://env.example" {
		t.Fatalf("env: %v %v %v", d, src, err)
	}
	d, src, err = ResolveDeployment("", "")
	if err != nil || src != FromDefault || d.String() != DefaultURL {
		t.Fatalf("default: %v %v %v", d, src, err)
	}
	d, src, err = ResolveDeployment("", "MY.nofire.ai/")
	if err != nil || src != FromEnv || d.String() != DefaultURL {
		t.Fatalf("env equal to default: %v %v %v", d, src, err)
	}
	if _, _, err = ResolveDeployment("", "http://h"); err == nil {
		t.Fatal("bad env accepted")
	}
	if _, _, err = ResolveDeployment("ftp://h", ""); err == nil {
		t.Fatal("bad arg accepted")
	}
}

func TestURLSourceString(t *testing.T) {
	for s, want := range map[URLSource]string{
		FromArg:     "from the command line",
		FromEnv:     "from BRIG_NOFIRE_URL",
		FromDefault: "default",
	} {
		if got := s.String(); got != want {
			t.Errorf("%d: %q, want %q", s, got, want)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	d, err := ParseDeployment("https://my.nofire.ai")
	if err != nil {
		t.Fatal(err)
	}
	for raw, want := range map[string]bool{
		"https://my.nofire.ai/oauth/token":    true,
		"https://MY.nofire.ai:443/x?y=1":      true,
		"https://my.nofire.ai":                true,
		"https://my.nofire.ai:8443/x":         false,
		"http://my.nofire.ai/x":               false,
		"https://other.nofire.ai/x":           false,
		"https://my.nofire.ai.evil.example/x": false,
		"https://my.nofire.ai@evil.example/x": false,
		"/relative":                           false,
		"":                                    false,
	} {
		if got := d.SameOrigin(raw); got != want {
			t.Errorf("SameOrigin(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestSameOriginIPv6AndPrefix(t *testing.T) {
	d, err := ParseDeployment("http://[::1]:3000/prefix")
	if err != nil {
		t.Fatal(err)
	}
	for raw, want := range map[string]bool{
		"http://[::1]:3000/other": true,
		"http://[::1]:3001/x":     false,
		"http://[::2]:3000/x":     false,
		"https://[::1]:3000/x":    false,
	} {
		if got := d.SameOrigin(raw); got != want {
			t.Errorf("SameOrigin(%q) = %v, want %v", raw, got, want)
		}
	}
	p, err := ParseDeployment("https://h/prefix/")
	if err != nil {
		t.Fatal(err)
	}
	if !p.SameOrigin("https://h/elsewhere") || p.SameOrigin("https://h2/prefix") {
		t.Error("prefixed deployment origin wrong")
	}
}

func TestParseDeploymentMessagesNameTheProblem(t *testing.T) {
	_, err := ParseDeployment("https://h/?q=1")
	if err == nil || !strings.Contains(err.Error(), "query") {
		t.Errorf("got %v", err)
	}
}
