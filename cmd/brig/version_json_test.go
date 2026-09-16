package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/buildinfo"
)

// brig version --json is the same build as the line, under the envelope every
// read verb prints. The flag works in both positions, and after either
// spelling of the verb.
func TestVersionJSONCarriesTheBuild(t *testing.T) {
	info := buildinfo.Read()
	for _, args := range [][]string{
		{"version", "--json"},
		{"--json", "version"},
		{"--version", "--json"},
		{"--json", "--version"},
	} {
		typed := strings.Join(args, " ")
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Fatalf("brig %s: %v", typed, err)
		}
		var doc struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Data       struct {
				Version    string `json:"version"`
				Commit     string `json:"commit"`
				CommitTime string `json:"commitTime"`
				Modified   bool   `json:"modified"`
				GoVersion  string `json:"goVersion"`
				OS         string `json:"os"`
				Arch       string `json:"arch"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("brig %s did not parse: %v\n%s", typed, err, out)
		}
		if doc.APIVersion != jsonAPIVersion || doc.Kind != "Version" {
			t.Errorf("brig %s envelope = %s/%s, want %s/Version", typed, doc.APIVersion, doc.Kind, jsonAPIVersion)
		}
		d := doc.Data
		if d.Version != info.Version || d.Commit != info.Commit || d.Modified != info.Modified ||
			d.GoVersion != info.GoVersion || d.OS != info.OS || d.Arch != info.Arch {
			t.Errorf("brig %s data = %+v, want the build %+v", typed, d, info)
		}
		// A build without VCS has no commit time, and the field is then absent
		// rather than a zero time a consumer would have to recognise.
		if info.CommitTime.IsZero() != !strings.Contains(out, `"commitTime"`) {
			t.Errorf("brig %s: commitTime present=%v, want present only when known:\n%s",
				typed, strings.Contains(out, `"commitTime"`), out)
		}
	}
}

// --json is the only word brig version takes; anything else is still refused.
func TestVersionRefusesOtherFlags(t *testing.T) {
	for _, args := range [][]string{
		{"version", "--json", "extra"},
		{"version", "--quiet"},
		{"--version", "-v"},
	} {
		var err error
		captureStderr(t, func() {
			_, err = captureStdout(t, func() error { return run(args) })
		})
		if err == nil {
			t.Errorf("brig %s was accepted, want a usage error", strings.Join(args, " "))
		}
	}
}
