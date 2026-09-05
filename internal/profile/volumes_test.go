package profile

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const claudeVolumes = `
volumes:
  - kind: hostmount
    path: .claude/history.jsonl
    file: true
  - kind: tmpfs
    path: .claude
  - kind: hostmount
    path: .claude/sessions
`

// The reference profile's own list, written with a child above its parent on
// purpose: file order is taste, and a reader must not have to keep the mount
// sequence in their head while writing one.
func TestVolumesParse(t *testing.T) {
	var p Profile
	if err := yaml.UnmarshalStrict([]byte(claudeVolumes), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Volumes) != 3 {
		t.Fatalf("got %+v, want three volumes", p.Volumes)
	}
	if p.Volumes[0].Kind != VolumeHostMount || !p.Volumes[0].File {
		t.Errorf("volumes[0] = %+v", p.Volumes[0])
	}
}

// Mount order is derived, NEVER declaration order. A profile listing
// .claude/sessions above .claude would otherwise mount the tmpfs over the
// hostmount it had just made, and silently lose the state -- a failure with no
// error and no symptom until someone goes looking for last week's sessions.
func TestMountOrderPutsParentsFirst(t *testing.T) {
	var p Profile
	if err := yaml.UnmarshalStrict([]byte(claudeVolumes), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := MountOrder(p.Volumes)
	if got[0].Path != ".claude" {
		t.Fatalf("MountOrder puts %q first, want .claude", got[0].Path)
	}
	// Stable within a depth: the two hostmounts keep the order they were
	// written in, so nothing reorders for reasons the author cannot see.
	if got[1].Path != ".claude/history.jsonl" || got[2].Path != ".claude/sessions" {
		t.Errorf("MountOrder = %q, %q", got[1].Path, got[2].Path)
	}
}

// MountOrder must not sort the profile's own slice under it: the registry
// hands profiles out by value and a shared backing array would reorder every
// later caller's list as a side effect of one delivery.
func TestMountOrderDoesNotReorderTheProfile(t *testing.T) {
	p := Profile{Volumes: []Volume{
		{Kind: VolumeHostMount, Path: ".claude/sessions"},
		{Kind: VolumeTmpfs, Path: ".claude"},
	}}
	MountOrder(p.Volumes)
	if p.Volumes[0].Path != ".claude/sessions" {
		t.Errorf("the profile's own list was reordered: %+v", p.Volumes)
	}
}

func TestUnderIsNestingNotEquality(t *testing.T) {
	if !Under(".claude/sessions", ".claude") {
		t.Error(".claude/sessions is not under .claude")
	}
	if Under(".claude", ".claude") {
		t.Error("a path is under itself; a hostmount replacing its tmpfs is not a hole in it")
	}
	if Under(".claude-backup", ".claude") {
		t.Error("a shared prefix counted as nesting")
	}
}

// EphemeralPath is the safety argument for the whole design, so it is pinned
// rather than left to the validation that consumes it.
func TestEphemeralPath(t *testing.T) {
	p := Profile{Volumes: []Volume{
		{Kind: VolumeTmpfs, Path: ".claude"},
		{Kind: VolumeHostMount, Path: ".claude/sessions"},
	}}
	cases := map[string]bool{
		".claude/.credentials.json":    true,
		".claude":                      true,
		".claude.json":                 false, // a sibling, not a child
		".claude/sessions":             false, // carved back out to the host
		".claude/sessions/a/cred.json": false, // and everything under it
		"cred.json":                    false,
	}
	for target, want := range cases {
		if got := p.EphemeralPath(target); got != want {
			t.Errorf("EphemeralPath(%q) = %v, want %v", target, got, want)
		}
	}
}

func TestFileModeDefaultsAndParses(t *testing.T) {
	if got, err := (FileBinding{}).FileMode(); err != nil || got != DefaultFileMode {
		t.Errorf("FileMode() = %v, %v; want 0600", got, err)
	}
	if got, err := (FileBinding{Mode: "0640"}).FileMode(); err != nil || got != 0o640 {
		t.Errorf("FileMode(0640) = %v, %v", got, err)
	}
	// "600" would parse as octal 0600 either way, but 0o1130 -- what YAML
	// gives a bare 0600 read as decimal -- must not be accepted as a mode.
	if _, err := (FileBinding{Mode: "1130"}).FileMode(); err == nil {
		t.Error("a mode above 0777 was accepted")
	}
	if _, err := (FileBinding{Mode: "rw-------"}).FileMode(); err == nil {
		t.Error("a symbolic mode was accepted")
	}
}

func TestVolumeAndFileValidation(t *testing.T) {
	tmpfs := Volume{Kind: VolumeTmpfs, Path: ".claude"}
	secrets := []SecretDecl{{Name: "cred"}}
	cases := []struct {
		name string
		p    Profile
		want string
	}{
		{"reserved kind is refused by name", Profile{Volumes: []Volume{
			{Kind: VolumeNamed, Path: ".claude/plugins", Source: "shared"}}}, "not yet supported"},
		{"unknown kind", Profile{Volumes: []Volume{
			{Kind: "bind", Path: ".claude"}}}, "bind"},
		{"no kind", Profile{Volumes: []Volume{{Path: ".claude"}}}, "kind"},
		{"absolute path", Profile{Volumes: []Volume{
			{Kind: VolumeTmpfs, Path: "/home/claude/.claude"}}}, "absolute"},
		{"escaping path", Profile{Volumes: []Volume{
			{Kind: VolumeTmpfs, Path: "../elsewhere"}}}, "escapes"},
		{"unclean path", Profile{Volumes: []Volume{
			{Kind: VolumeTmpfs, Path: "./.claude/"}}}, "simplest form"},
		{"duplicate paths", Profile{Volumes: []Volume{tmpfs, tmpfs}}, "twice"},
		{"source on a hostmount", Profile{Volumes: []Volume{tmpfs,
			{Kind: VolumeHostMount, Path: ".claude/sessions", Source: "elsewhere"}}}, "source"},
		{"file on a tmpfs", Profile{Volumes: []Volume{
			{Kind: VolumeTmpfs, Path: ".claude", File: true}}}, "file"},
		// The one that reads as protection and is not: the workspace is
		// already guestHome, so this mounts a path onto itself.
		{"hostmount under no tmpfs", Profile{Volumes: []Volume{
			{Kind: VolumeHostMount, Path: ".claude/sessions"}}}, "onto itself"},
		{"volumes and statePaths together", Profile{
			Volumes: []Volume{tmpfs}, StatePaths: []string{".claude"}}, "statePaths"},
		{"the reference shape is accepted", Profile{Volumes: []Volume{
			{Kind: VolumeHostMount, Path: ".claude/sessions"}, tmpfs}}, ""},

		{"file with no volumes at all", Profile{Secrets: secrets, Files: []FileBinding{
			{Ref: "secrets.cred", Path: ".claude/.credentials.json"}}}, "host disk"},
		{"file carved back out by a hostmount", Profile{Secrets: secrets,
			Volumes: []Volume{tmpfs, {Kind: VolumeHostMount, Path: ".claude/sessions"}},
			Files: []FileBinding{
				{Ref: "secrets.cred", Path: ".claude/sessions/cred.json"}}}, "host disk"},
		{"file refs an undeclared secret", Profile{Volumes: []Volume{tmpfs}, Files: []FileBinding{
			{Ref: "secrets.nope", Path: ".claude/.credentials.json"}}}, "secrets:"},
		{"file refs the environment", Profile{Secrets: secrets, Volumes: []Volume{tmpfs},
			Files: []FileBinding{
				{Ref: "env.TOKEN", Path: ".claude/.credentials.json"}}}, "own store"},
		{"file with no ref", Profile{Volumes: []Volume{tmpfs}, Files: []FileBinding{
			{Path: ".claude/.credentials.json"}}}, "ref"},
		{"two files at one path", Profile{Secrets: secrets, Volumes: []Volume{tmpfs}, Files: []FileBinding{
			{Ref: "secrets.cred", Path: ".claude/.credentials.json"},
			{Ref: "secrets.cred", Path: ".claude/.credentials.json"}}}, "twice"},
		{"bad mode", Profile{Secrets: secrets, Volumes: []Volume{tmpfs}, Files: []FileBinding{
			{Ref: "secrets.cred", Path: ".claude/.credentials.json", Mode: "junk"}}}, "octal"},
		{"the reference file binding is accepted", Profile{Secrets: secrets,
			Volumes: []Volume{tmpfs}, Files: []FileBinding{
				{Ref: "secrets.cred", Path: ".claude/.credentials.json", Mode: "0600"}}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.p.validateBindings()
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("valid profile rejected: %v", err)
			case c.want == "":
			case err == nil:
				t.Fatalf("accepted, want an error naming %q", c.want)
			case !strings.Contains(err.Error(), c.want):
				t.Errorf("error %v does not name %q", err, c.want)
			}
		})
	}
}

// Binding one secret through both channels is legal, and sometimes correct:
// brig's git credential helper could read a file while the gh CLI inside the
// guest reads the variable and will not look anywhere else. No rule may forbid
// it -- a warning here would fire on a correct configuration.
func TestOneSecretMayTakeBothChannels(t *testing.T) {
	p := Profile{
		Secrets: []SecretDecl{{Name: "gh-token"}},
		Volumes: []Volume{{Kind: VolumeTmpfs, Path: ".config"}},
		Files:   []FileBinding{{Ref: "secrets.gh-token", Path: ".config/gh-token"}},
		Env:     []EnvBinding{{Name: "GH_TOKEN", Ref: "secrets.gh-token"}},
	}
	if err := p.validateBindings(); err != nil {
		t.Errorf("one secret through both channels was rejected: %v", err)
	}
}

// A clone that shares the volume or file slice hands the registry's own
// profile to whoever appends to one -- and what these two lists decide is
// where a credential lands.
func TestCloneDoesNotShareVolumesOrFiles(t *testing.T) {
	p := Profile{
		Volumes: []Volume{{Kind: VolumeTmpfs, Path: ".claude"}},
		Files:   []FileBinding{{Ref: "secrets.cred", Path: ".claude/.credentials.json"}},
	}
	c := p.clone()
	c.Volumes[0].Path = ".elsewhere"
	c.Files[0].Path = ".elsewhere/cred"
	if p.Volumes[0].Path != ".claude" || p.Files[0].Path != ".claude/.credentials.json" {
		t.Error("a clone shares the original's volumes or files")
	}
}
