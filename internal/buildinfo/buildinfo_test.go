package buildinfo

import (
	"runtime/debug"
	"testing"
	"time"
)

// A binary built in a git checkout at a tag: Go stamps the tag as the module
// version and the commit beside it.
func TestFromReadsATaggedBuild(t *testing.T) {
	got := From(&debug.BuildInfo{
		GoVersion: "go1.26.0",
		Main:      debug.Module{Path: "github.com/brig-sh/brig", Version: "v0.3.0"},
		Settings: []debug.BuildSetting{
			{Key: "GOOS", Value: "darwin"},
			{Key: "GOARCH", Value: "arm64"},
			{Key: "vcs", Value: "git"},
			{Key: "vcs.revision", Value: "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"},
			{Key: "vcs.time", Value: "2026-09-16T12:00:00Z"},
			{Key: "vcs.modified", Value: "false"},
		},
	})
	want := Info{
		Version:    "v0.3.0",
		Commit:     "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2",
		CommitTime: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
		GoVersion:  "go1.26.0",
		OS:         "darwin",
		Arch:       "arm64",
	}
	if got != want {
		t.Errorf("From() = %+v, want %+v", got, want)
	}
}

// Uncommitted changes: Go already suffixes the version with +dirty; the flag is
// reported on its own as well so a consumer need not parse the string.
func TestFromReportsAModifiedTree(t *testing.T) {
	got := From(&debug.BuildInfo{
		Main: debug.Module{Version: "v0.3.1-0.20260916120000-b2b2b2b2b2b2+dirty"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"},
			{Key: "vcs.time", Value: "2026-09-16T12:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	})
	if !got.Modified {
		t.Errorf("Modified = false, want true")
	}
	if got.Version != "v0.3.1-0.20260916120000-b2b2b2b2b2b2+dirty" {
		t.Errorf("Version = %q, want the pseudo-version with +dirty kept", got.Version)
	}
}

// A build with no VCS -- a source tarball, or `go install` from the module
// proxy -- has no commit to report. The version is what Go knows, and "(devel)"
// is spelled dev.
func TestFromWithoutVCS(t *testing.T) {
	proxy := From(&debug.BuildInfo{Main: debug.Module{Version: "v0.3.0"}})
	if proxy.Version != "v0.3.0" || proxy.Commit != "" || !proxy.CommitTime.IsZero() {
		t.Errorf("proxy build = %+v, want version v0.3.0 and no commit", proxy)
	}
	tarball := From(&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}})
	if tarball.Version != "dev" {
		t.Errorf("Version = %q, want dev for (devel)", tarball.Version)
	}
	none := From(nil)
	if none.Version != "dev" {
		t.Errorf("Version = %q, want dev when there is no build info at all", none.Version)
	}
}

// The Go version and platform fall back to the running binary's own when the
// build info does not carry them, so the line never shows an empty field.
func TestFromFallsBackToTheRuntimeForGoAndPlatform(t *testing.T) {
	got := From(&debug.BuildInfo{Main: debug.Module{Version: "v0.3.0"}})
	if got.GoVersion == "" || got.OS == "" || got.Arch == "" {
		t.Errorf("From() left a runtime field empty: %+v", got)
	}
}

// The one-line human form: version, then the short commit, the commit date, the
// Go version and the platform in parentheses. Fields the build does not have
// are left out rather than printed empty.
func TestString(t *testing.T) {
	for _, c := range []struct {
		name string
		info Info
		want string
	}{
		{"tagged", Info{
			Version: "v0.3.0", Commit: "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2",
			CommitTime: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
			GoVersion:  "go1.26.0", OS: "darwin", Arch: "arm64",
		}, "v0.3.0 (b2b2b2b, 2026-09-16, go1.26.0, darwin/arm64)"},
		{"pseudo-version", Info{
			Version: "v0.3.1-0.20260916120000-b2b2b2b2b2b2", Commit: "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2",
			CommitTime: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
			GoVersion:  "go1.26.0", OS: "linux", Arch: "amd64",
		}, "v0.3.1-0.20260916120000-b2b2b2b2b2b2 (b2b2b2b, 2026-09-16, go1.26.0, linux/amd64)"},
		{"no vcs", Info{Version: "dev", GoVersion: "go1.26.0", OS: "darwin", Arch: "arm64"},
			"dev (go1.26.0, darwin/arm64)"},
		{"commit date in UTC", Info{
			Version: "v0.3.0", Commit: "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2",
			CommitTime: time.Date(2026, 9, 16, 23, 30, 0, 0, time.FixedZone("west", -3*3600)),
			GoVersion:  "go1.26.0", OS: "darwin", Arch: "arm64",
		}, "v0.3.0 (b2b2b2b, 2026-09-17, go1.26.0, darwin/arm64)"},
	} {
		if got := c.info.String(); got != c.want {
			t.Errorf("%s: String() = %q, want %q", c.name, got, c.want)
		}
	}
}

// A build the toolchain embedded no VCS data into -- a linked worktree -- takes
// git's answers from the Makefile, and names the same version a build from a
// normal clone of that commit would.
func TestWithGitDerivesTheToolchainVersion(t *testing.T) {
	const commit = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	const at = "2026-09-16T14:00:00+02:00"
	for _, c := range []struct {
		name     string
		describe string
		modified bool
		want     string
	}{
		{"tagged", "v0.3.0-0-gb2b2b2b", false, "v0.3.0"},
		{"tagged prerelease", "v0.4.0-rc1-0-gb2b2b2b", false, "v0.4.0-rc1"},
		{"after a release", "v0.3.0-6-gb2b2b2b", false, "v0.3.1-0.20260916120000-b2b2b2b2b2b2"},
		{"after a prerelease", "v0.4.0-rc1-2-gb2b2b2b", false, "v0.4.0-rc1.0.20260916120000-b2b2b2b2b2b2"},
		{"no tag", "", false, "v0.0.0-20260916120000-b2b2b2b2b2b2"},
		{"a tag that is not semver", "release-1-gb2b2b2b", false, "v0.0.0-20260916120000-b2b2b2b2b2b2"},
		{"modified", "v0.3.0-0-gb2b2b2b", true, "v0.3.0+dirty"},
	} {
		got := Info{Version: "dev"}.withGit(commit, at, c.describe, c.modified)
		if got.Version != c.want {
			t.Errorf("%s: Version = %q, want %q", c.name, got.Version, c.want)
		}
		if got.Commit != commit || !got.CommitTime.Equal(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)) || got.Modified != c.modified {
			t.Errorf("%s: withGit() = %+v, want the commit, its time and modified=%v", c.name, got, c.modified)
		}
	}
}

// Without git's answers -- any build not made by the Makefile -- nothing is
// filled in, and a version the toolchain did derive is never replaced.
func TestWithGitKeepsWhatItCannotImprove(t *testing.T) {
	none := Info{Version: "dev"}.withGit("", "", "", false)
	if none != (Info{Version: "dev"}) {
		t.Errorf("withGit with no answers = %+v, want the build unchanged", none)
	}
	proxy := Info{Version: "v0.3.0"}.withGit("b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2", "2026-09-16T12:00:00Z", "v0.2.0-9-gb2b2b2b", false)
	if proxy.Version != "v0.3.0" {
		t.Errorf("Version = %q, want the toolchain's v0.3.0 kept", proxy.Version)
	}
}
