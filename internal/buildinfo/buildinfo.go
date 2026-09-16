// Package buildinfo reports which build a binary is, from what the Go
// toolchain embeds in it.
//
// Nothing is stamped at link time. Go 1.24 and later derive the main module
// version from the nearest reachable v* tag -- the tag itself on a tagged
// commit, a pseudo-version such as v0.3.1-0.20260916120000-b2b2b2b2b2b2 after
// one, and a +dirty suffix when the tree had uncommitted changes -- and record
// the commit, its time and the platform beside it. Reading those is what lets
// two binaries from the same tag be told apart, and lets a plain `go build`
// or `go install` say more than "dev".
//
// The one exception is a linked git worktree. Its .git is a file, which the
// toolchain does not recognise as a repository, so it embeds no VCS data at
// all. The Makefile passes git's own answers in the variables below, and Read
// uses them only when the toolchain embedded nothing. goreleaser passes none.
package buildinfo

import (
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Set with -X by the Makefile; empty in every other build.
var (
	gitCommit     string // git rev-parse HEAD
	gitCommitTime string // git log -1 --format=%cI
	gitDescribe   string // git describe --tags --long --match 'v[0-9]*'
	gitModified   string // "true" when git status --porcelain printed anything
)

// Info is one binary's build.
type Info struct {
	// Version is the module version Go derived: a tag, a pseudo-version, or
	// "dev" when the build carried none.
	Version string
	// Commit is the full revision, empty when the build had no VCS data: a
	// source tarball, or `go install` from the module proxy.
	Commit string
	// CommitTime is the commit's time, zero when Commit is empty.
	CommitTime time.Time
	// Modified is whether the tree had uncommitted changes.
	Modified  bool
	GoVersion string
	OS        string
	Arch      string
}

// Read is the running binary's build.
func Read() Info {
	bi, _ := debug.ReadBuildInfo()
	info := From(bi)
	if info.Commit == "" {
		info = info.withGit(gitCommit, gitCommitTime, gitDescribe, gitModified == "true")
	}
	return info
}

// From reads one debug.BuildInfo. Fields the build info lacks fall back to
// the running binary's own toolchain and platform, so none is left empty; a
// nil build info yields "dev" with only those.
func From(bi *debug.BuildInfo) Info {
	info := Info{Version: "dev", GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if bi == nil {
		return info
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		info.Version = v
	}
	if bi.GoVersion != "" {
		info.GoVersion = bi.GoVersion
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Commit = s.Value
		case "vcs.time":
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				info.CommitTime = t
			}
		case "vcs.modified":
			info.Modified = s.Value == "true"
		case "GOOS":
			info.OS = s.Value
		case "GOARCH":
			info.Arch = s.Value
		}
	}
	return info
}

// withGit fills in a build the toolchain embedded no VCS data into from git's
// answers, and derives the version the way the toolchain would have: the tag
// on a tagged commit, a pseudo-version after one, +dirty on a modified tree.
// A version the toolchain did derive is kept. Without a commit and its time
// there is nothing to fill in from.
func (i Info) withGit(commit, commitTime, describe string, modified bool) Info {
	t, err := time.Parse(time.RFC3339, commitTime)
	if commit == "" || err != nil {
		return i
	}
	i.Commit, i.CommitTime, i.Modified = commit, t, modified
	if i.Version == "dev" {
		i.Version = versionFromDescribe(describe, t, commit)
		if modified {
			i.Version += "+dirty"
		}
	}
	return i
}

var (
	// describeRE splits `git describe --long` into the tag and the number of
	// commits since it.
	describeRE = regexp.MustCompile(`^(.+)-([0-9]+)-g[0-9a-f]+$`)
	// semverRE is a tag the toolchain takes as a version of this module:
	// canonical semver, major 0 or 1 because the module path has no /vN.
	semverRE = regexp.MustCompile(`^v[01]\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)
)

// versionFromDescribe is the version for a commit given its nearest tag. git
// describe picks the nearest tag where the toolchain picks the highest one
// reachable; on linear history they are the same tag.
func versionFromDescribe(describe string, t time.Time, commit string) string {
	suffix := t.UTC().Format("20060102150405") + "-" + commit[:min(12, len(commit))]
	m := describeRE.FindStringSubmatch(describe)
	if m == nil || !semverRE.MatchString(m[1]) {
		return "v0.0.0-" + suffix
	}
	tag := m[1]
	switch {
	case m[2] == "0":
		return tag
	case strings.Contains(tag, "-"):
		return tag + ".0." + suffix
	default:
		dot := strings.LastIndexByte(tag, '.')
		patch, _ := strconv.Atoi(tag[dot+1:])
		return fmt.Sprintf("%s%d-0.%s", tag[:dot+1], patch+1, suffix)
	}
}

// String is the one-line form: the version, then in parentheses the short
// commit and its date when the build has them, the Go version, and the
// platform.
//
//	v0.3.0 (b2b2b2b, 2026-09-16, go1.26.0, darwin/arm64)
//	dev (go1.26.0, darwin/arm64)
func (i Info) String() string {
	var parts []string
	if i.Commit != "" {
		parts = append(parts, ShortCommit(i.Commit))
	}
	if !i.CommitTime.IsZero() {
		parts = append(parts, i.CommitTime.UTC().Format(time.DateOnly))
	}
	parts = append(parts, i.GoVersion, i.OS+"/"+i.Arch)
	return fmt.Sprintf("%s (%s)", i.Version, strings.Join(parts, ", "))
}

// ShortCommit is the first seven characters of a revision, the length git
// itself abbreviates to.
func ShortCommit(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}
