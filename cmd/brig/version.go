package main

import (
	"fmt"
	"os"
	"time"

	"github.com/brig-sh/brig/internal/buildinfo"
)

// versionCmd is `brig version` and `brig --version`: one line naming the
// build, or the same build under the --json envelope. Named as the reader
// spelled it. Both spellings are current -- one is not a retirement of the
// other -- so there is no newer word to send them to, unlike the deprecated
// listings.
func versionCmd(verb string, rest []string) error {
	wantJSON := globalJSON
	for _, a := range rest {
		if a != "--json" {
			return usagef("unexpected argument %q; `brig %s` takes only --json", a, verb)
		}
		wantJSON = true
	}
	info := buildinfo.Read()
	if wantJSON {
		return writeJSONDocument(os.Stdout, "Version", versionData(info))
	}
	fmt.Printf("brig %s\n", info)
	return nil
}

// versionPayload is the data of a Version document: the same fields the line
// prints, with the commit in full. commit and commitTime are absent, not
// empty, when the build carried no VCS data, so a consumer tests for the key
// rather than recognising a zero time.
type versionPayload struct {
	Version    string `json:"version"`
	Commit     string `json:"commit,omitempty"`
	CommitTime string `json:"commitTime,omitempty"`
	Modified   bool   `json:"modified"`
	GoVersion  string `json:"goVersion"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
}

func versionData(info buildinfo.Info) versionPayload {
	p := versionPayload{
		Version:   info.Version,
		Commit:    info.Commit,
		Modified:  info.Modified,
		GoVersion: info.GoVersion,
		OS:        info.OS,
		Arch:      info.Arch,
	}
	if !info.CommitTime.IsZero() {
		p.CommitTime = info.CommitTime.UTC().Format(time.RFC3339)
	}
	return p
}
