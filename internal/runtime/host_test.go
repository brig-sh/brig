package runtime

import (
	"errors"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
)

func testPolicy(net string, ports ...int) sandboxPolicy {
	return sandboxPolicy{
		Net:           net,
		LoopbackPorts: ports,
		StateDir:      "/Users/me/.brig/host",
		SecretDir:     "/Users/me/.brig/host/brig-x/secret",
	}
}

// linesWith returns the profile lines that start with prefix.
func linesWith(profile, prefix string) []string {
	var out []string
	for _, l := range strings.Split(profile, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

// The network part of the profile, one case per mode. allows is every
// network allow the profile may carry, and nothing else may appear.
func TestSandboxProfileNetworkAllows(t *testing.T) {
	resolver := `(allow network-outbound (remote unix-socket (path-literal "/private/var/run/mDNSResponder")))`
	for _, tt := range []struct {
		name     string
		policy   sandboxPolicy
		denies   []string
		allows   []string
		noIPDeny bool
	}{
		{
			name:   "isolated with one loopback port",
			policy: testPolicy("isolated", 48711),
			denies: []string{"(deny network-outbound)"},
			allows: []string{resolver, `(allow network-outbound (remote ip "localhost:48711"))`},
		},
		{
			name:   "isolated with the port named twice",
			policy: testPolicy("isolated", 48711, 48711),
			denies: []string{"(deny network-outbound)"},
			allows: []string{resolver, `(allow network-outbound (remote ip "localhost:48711"))`},
		},
		{
			name:   "isolated with no port",
			policy: testPolicy("isolated"),
			denies: []string{"(deny network-outbound)"},
			allows: []string{resolver},
		},
		{
			name:   "offline",
			policy: testPolicy("none", 48711),
			denies: []string{"(deny network-outbound)"},
		},
		{
			name:     "shared",
			policy:   testPolicy("shared"),
			denies:   []string{"(deny network-outbound (remote unix-socket))"},
			allows:   []string{resolver},
			noIPDeny: true,
		},
		{
			// A policy takes shared away, so the network is denied as on
			// isolated.
			name:   "shared under a policy",
			policy: sandboxPolicy{Net: "shared", Filtered: true, StateDir: "/s", SecretDir: "/s/x/secret"},
			denies: []string{"(deny network-outbound)"},
			allows: []string{resolver},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sandboxProfile(tt.policy)
			if err != nil {
				t.Fatal(err)
			}
			have := linesWith(got, "(allow network")
			if !slices.Equal(have, tt.allows) {
				t.Errorf("network allows = %q, want exactly %q\n%s", have, tt.allows, got)
			}
			if n := len(linesWith(got, `(allow network-outbound (remote ip`)); n > 1 {
				t.Errorf("%d loopback allows, want at most one:\n%s", n, got)
			}
			for _, d := range tt.denies {
				if !slices.Contains(linesWith(got, "(deny network"), d) {
					t.Errorf("missing %q:\n%s", d, got)
				}
			}
			if tt.noIPDeny && slices.Contains(linesWith(got, "(deny network"), "(deny network-outbound)") {
				t.Errorf("shared denies IP egress:\n%s", got)
			}
		})
	}
}

// nsurlsessiond opens a background download outside the sandbox, so every
// posture but plain shared denies it with the network.
func TestSandboxProfileDeniesTheDownloadDaemonOffShared(t *testing.T) {
	const deny = `(deny mach-lookup (global-name "com.apple.nsurlsessiond") (global-name "com.apple.nsurlsessiond-launchondemand"))`
	for _, tt := range []struct {
		name   string
		policy sandboxPolicy
		want   bool
	}{
		{"isolated", testPolicy("isolated", 48711), true},
		{"offline", testPolicy("none"), true},
		{"shared under a policy", sandboxPolicy{Net: "shared", Filtered: true, StateDir: "/s", SecretDir: "/s/x/secret"}, true},
		{"shared", testPolicy("shared"), false},
	} {
		got, err := sandboxProfile(tt.policy)
		if err != nil {
			t.Fatal(err)
		}
		if have := slices.Contains(strings.Split(got, "\n"), deny); have != tt.want {
			t.Errorf("%s: the download daemon denied = %v, want %v:\n%s", tt.name, have, tt.want, got)
		}
	}
}

// The file and keychain denies are in every profile, and after every allow,
// because a later rule wins in Seatbelt.
func TestSandboxProfileAlwaysDeniesTheSensitivePaths(t *testing.T) {
	for _, net := range []string{"isolated", "none", "shared"} {
		p := testPolicy(net, 48711)
		p.Protect = []string{"/Users/me/.brig", "/Users/me/.config/brig"}
		p.Homes = "/Users/me/.brig/homes"
		p.Shares = []string{"/Users/me/.brig/homes/brig-x"}
		p.Ancestors = []string{"/Users/me/.config"}
		got, err := sandboxProfile(p)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(got), "\n")
		lastAllow := -1
		for i, l := range lines {
			if strings.HasPrefix(l, "(allow") {
				lastAllow = i
			}
		}
		for _, want := range []string{
			`(deny file-write* (literal "/Users/me/.config"))`,
			`(deny file-read* file-write* (regex #"^/dev/bpf[0-9]+$"))`,
			`(deny mach-lookup (global-name "com.apple.SecurityServer"))`,
			`(deny mach-lookup (global-name "com.apple.pasteboard.1"))`,
			`(deny file-ioctl (ioctl-command TIOCSTI))`,
			`(deny file-mount)`,
			`(deny file-unmount)`,
			`(deny mach-lookup (global-name "com.apple.DiskArbitration.diskarbitrationd") (global-name "com.apple.DiskArbitration.DiskArbitrationAgent"))`,
			`(deny file-read* file-write* (subpath "` + p.SecretDir + `"))`,
			`(deny file-read* file-write* (subpath "` + p.StateDir + `"))`,
		} {
			i := slices.Index(lines, want)
			if i < 0 {
				t.Errorf("%s: missing %s:\n%s", net, want, got)
				continue
			}
			if i < lastAllow {
				t.Errorf("%s: %s comes before an allow, which would win over it:\n%s", net, want, got)
			}
		}
		for _, want := range []string{"(deny network-inbound)", "(deny lsopen)", "(deny appleevent-send)",
			"(deny signal)", "(allow signal (target same-sandbox))"} {
			if !slices.Contains(lines, want) {
				t.Errorf("%s: missing %s:\n%s", net, want, got)
			}
		}
		// The home is inside brig's state directory and the homes directory,
		// so its allow has to come after the denies on both to win over them.
		// The homes directory may be looked up, and nothing more.
		protect := slices.Index(lines, `(deny file-write* (subpath "/Users/me/.brig"))`)
		homes := slices.Index(lines, `(deny file-read* file-write* (subpath "/Users/me/.brig/homes"))`)
		lookup := slices.Index(lines, `(allow file-read-metadata (literal "/Users/me/.brig/homes"))`)
		share := slices.Index(lines, `(allow file-read* file-write* (subpath "/Users/me/.brig/homes/brig-x"))`)
		if protect < 0 || homes < protect || lookup < homes || share < lookup {
			t.Errorf("%s: want the state directory deny (%d), the homes deny (%d), its lookup (%d), then the "+
				"home allow (%d):\n%s", net, protect, homes, lookup, share, got)
		}
	}
}

func TestSandboxProfileRefusesWhatItCannotWrite(t *testing.T) {
	for _, tt := range []struct {
		name   string
		policy sandboxPolicy
	}{
		{"port zero", testPolicy("isolated", 0)},
		{"port past 65535", testPolicy("isolated", 70000)},
		{"a quote in a path", sandboxPolicy{Net: "none", StateDir: `/Users/me/.brig/host") (allow default`, SecretDir: "/s"}},
		{"a newline in a path", sandboxPolicy{Net: "none", StateDir: "/s", SecretDir: "/s/x\n(allow default)"}},
		{"a relative path", sandboxPolicy{Net: "none", StateDir: "s", SecretDir: "/s"}},
		// %q writes these as escapes, which Seatbelt reads as they are, so the
		// deny would name another path.
		{"a no-break space in a protected path", sandboxPolicy{Net: "none", StateDir: "/s", SecretDir: "/s/x",
			Protect: []string{"/Users/me/.config/nb sp"}}},
		{"a zero-width joiner in a path", sandboxPolicy{Net: "none", StateDir: "/s/zw‍j", SecretDir: "/s/x"}},
		{"a C1 control in a path", sandboxPolicy{Net: "none", StateDir: "/s", SecretDir: "/s/x\u0085"}},
		{"invalid UTF-8 in a path", sandboxPolicy{Net: "none", StateDir: "/s", SecretDir: "/s",
			Shares: []string{"/Users/me/caf\xe9"}}},
	} {
		if got, err := sandboxProfile(tt.policy); err == nil {
			t.Errorf("%s: written anyway:\n%s", tt.name, got)
		}
	}
	// A printable rune that is not ASCII is written as it is.
	p := sandboxPolicy{Net: "none", StateDir: "/s", SecretDir: "/s/x", Protect: []string{"/Users/me/café"}}
	if got, err := sandboxProfile(p); err != nil || !strings.Contains(got, `(subpath "/Users/me/café")`) {
		t.Errorf("a path with é: %v\n%s", err, got)
	}
}

// A policy with a host or cidr rule cannot be held by the process sandbox, so
// it is refused before anything is written, naming the kind and the reason.
func TestHostRefusesAPolicyWithRules(t *testing.T) {
	for _, tt := range []struct {
		name   string
		spec   RunSpec
		refuse bool
	}{
		{"a hostname rule", RunSpec{Net: "isolated", Egress: Egress{Default: "deny", Allow: []Rule{{Host: "api.anthropic.com"}}}}, true},
		{"a cidr rule", RunSpec{Net: "isolated", Egress: Egress{Default: "allow", Deny: []Rule{{CIDR: "10.0.0.0/8"}}}}, true},
		{"a policy on shared", RunSpec{Net: "shared", Egress: Egress{Default: "deny"}}, true},
		{"default deny with no rule", RunSpec{Net: "isolated", Egress: Egress{Default: "deny"}}, false},
		{"a rule, but offline", RunSpec{Net: "none", Egress: Egress{Default: "deny", Allow: []Rule{{Host: "example.com"}}}}, false},
		{"no policy", RunSpec{Net: "isolated"}, false},
	} {
		err := hostSupports(tt.spec)
		if !tt.refuse {
			if err != nil {
				t.Errorf("%s: refused: %v", tt.name, err)
			}
			continue
		}
		var refusal *CapabilityError
		if !errors.As(err, &refusal) {
			t.Fatalf("%s: got %v, want a CapabilityError", tt.name, err)
		}
		if refusal.State != CannotEnforce {
			t.Errorf("%s: state %q, want %q", tt.name, refusal.State, CannotEnforce)
		}
		msg := err.Error()
		for _, want := range []string{"host cannot enforce the egress policy", "matches addresses, not names", "microVM"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: the refusal does not say %q: %s", tt.name, want, msg)
			}
		}
	}
}

func TestHostRefusesAWindowAPublishedPortAndATmpfs(t *testing.T) {
	if err := hostSupports(RunSpec{Net: "isolated", Tmpfs: []string{"/brig/claude:size=1m"}}); err == nil {
		t.Error("a tmpfs was accepted")
	}
	if err := hostSupports(RunSpec{Net: "isolated", GUI: true}); err == nil {
		t.Error("a graphical profile was accepted")
	}
	p, err := ParsePublication("8080:80")
	if err != nil {
		t.Fatal(err)
	}
	if err := hostSupports(RunSpec{Net: "isolated", Publish: []Publication{p}}); err == nil {
		t.Error("a published port was accepted")
	}
}

func TestHostPathMapsGuestPathsToShares(t *testing.T) {
	shares := []hostShare{
		{Guest: "/root", Host: "/Users/me/.brig/homes/brig-x"},
		{Guest: "/work/proj", Host: "/Users/me/src/proj"},
		{Guest: "/root/.cache", Host: "/Users/me/.brig/homes/brig-x/.cache-real"},
	}
	for in, want := range map[string]string{
		"/root":               "/Users/me/.brig/homes/brig-x",
		"/root/.claude":       "/Users/me/.brig/homes/brig-x/.claude",
		"/rootfs":             "/rootfs",
		"/work/proj/sub":      "/Users/me/src/proj/sub",
		"/work/project":       "/work/project",
		"/root/.cache/x":      "/Users/me/.brig/homes/brig-x/.cache-real/x",
		"https://example.com": "https://example.com",
		"":                    "",
	} {
		if got := hostPath(shares, in); got != want {
			t.Errorf("hostPath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := hostPathList(shares, "/root/.local/bin:/usr/bin"); got != "/Users/me/.brig/homes/brig-x/.local/bin:/usr/bin" {
		t.Errorf("PATH maps to %q", got)
	}
}

// The agent gets the variables brig names and the plumbing a process needs,
// and nothing else from the shell that ran brig.
func TestHostEnvForwardsOnlyWhatIsNamed(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-value")
	t.Setenv("TERM", "xterm-256color")
	shares := []hostShare{{Guest: "/root", Host: "/h"}}
	env := hostEnv([]Var{
		{Name: "HOME", Value: "/root"},
		{Name: "GH_TOKEN", Value: "/root/not-a-path-to-map", Secret: true},
	}, shares)

	for _, kv := range env {
		if strings.HasPrefix(kv, "AWS_SECRET_ACCESS_KEY=") || strings.Contains(kv, "ambient-value") {
			t.Errorf("an unnamed variable reached the agent: %s", kv)
		}
	}
	for _, want := range []string{"HOME=/h", "GH_TOKEN=/root/not-a-path-to-map", "TERM=xterm-256color"} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %s: %q", want, env)
		}
	}
}

func TestHostCommandFallsBackToTheNameOfAMissingPath(t *testing.T) {
	if got := hostCommand([]string{"/nonexistent/brig-true", "x"}); !slices.Equal(got, []string{"brig-true", "x"}) {
		t.Errorf("got %q", got)
	}
	if got := hostCommand([]string{"/bin/sh", "-c", "true"}); got[0] != "/bin/sh" {
		t.Errorf("an existing path was rewritten: %q", got)
	}
	if got := hostCommand([]string{"claude"}); got[0] != "claude" {
		t.Errorf("a bare name was rewritten: %q", got)
	}
}

func TestHostNameRefusesAPath(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", "../x", "brig x"} {
		if hostName(name) == nil {
			t.Errorf("%q accepted", name)
		}
	}
	if err := hostName("brig-claude-code.1_x"); err != nil {
		t.Error(err)
	}
}

// BRIG_RUNTIME=host and isolation: process select the host kind, and an unset
// field leaves the default alone.
func TestDetectForSelectsTheHostKind(t *testing.T) {
	bin := executableFile(t, t.TempDir(), "sandbox-exec")
	t.Setenv("BRIG_RUNTIME_BIN", bin)
	for _, tt := range []struct {
		name      string
		env       string
		isolation string
		want      string
	}{
		{"BRIG_RUNTIME=host", "host", "", HostKind},
		{"isolation: process", "", IsolationProcess, HostKind},
		{"isolation: microvm", "", IsolationMicroVM, defaultKind()},
		{"no isolation", "", "", defaultKind()},
		{"BRIG_RUNTIME=hull, no isolation", "hull", "", "hull"},
		{"BRIG_RUNTIME=hull, isolation: microvm", "hull", IsolationMicroVM, "hull"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BRIG_RUNTIME", tt.env)
			rt, err := DetectFor(Preference{Isolation: tt.isolation})
			if tt.want == HostKind && goruntime.GOOS != "darwin" {
				if err == nil || !strings.Contains(err.Error(), "unsupported on this platform") {
					t.Fatalf("got %v, want the host kind refused as unsupported", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if rt.Kind() != tt.want && !(tt.want == "nerdctl" && rt.Kind() == "docker") {
				t.Errorf("kind %q, want %q", rt.Kind(), tt.want)
			}
			if tt.want == HostKind {
				if got := rt.Isolation("hvi").Line(); got != "process (shared kernel)" {
					t.Errorf("isolation row %q", got)
				}
			}
		})
	}
}

func TestDetectForNamesTheHostKindWhenRefusingAnUnknownOne(t *testing.T) {
	t.Setenv("BRIG_RUNTIME", "podman")
	_, err := DetectFor(Preference{})
	if err == nil || !strings.Contains(err.Error(), "want hull, nerdctl or host") {
		t.Fatalf("got %v", err)
	}
}

// A variable that names the other boundary than the profile is refused,
// rather than one of the two picked without a word.
func TestDetectForRefusesAConflictWithTheProfile(t *testing.T) {
	t.Setenv("BRIG_RUNTIME_BIN", executableFile(t, t.TempDir(), "sandbox-exec"))
	for _, tt := range []struct{ env, isolation, want string }{
		{"host", IsolationMicroVM, "sets isolation: microvm"},
		{"hull", IsolationProcess, "sets isolation: process"},
		{"nerdctl", IsolationProcess, "sets isolation: process"},
	} {
		t.Setenv("BRIG_RUNTIME", tt.env)
		_, err := DetectFor(Preference{Isolation: tt.isolation})
		if !errors.Is(err, ErrBadRuntime) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("BRIG_RUNTIME=%s with isolation: %s: got %v", tt.env, tt.isolation, err)
		}
	}
}

// The broker keeps its directory under the host sandboxes', so no sandbox
// may take its name, and a listing never shows it as one.
func TestTheBrokerDirectoryIsNoSandbox(t *testing.T) {
	if err := hostName(HostBrokerDir); err == nil {
		t.Errorf("%q was accepted as a sandbox name", HostBrokerDir)
	}
	if err := hostName("brig-broker"); err != nil {
		t.Errorf("an ordinary name was refused: %v", err)
	}
}
