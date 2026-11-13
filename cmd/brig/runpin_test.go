package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// recordingRuntime is jsonRuntime with what a boot and an exec were handed
// kept, so a test can read what the runtime would have been told.
type recordingRuntime struct {
	jsonRuntime
	runs  []runtime.RunSpec
	execs []runtime.ExecSpec
}

func (r *recordingRuntime) Run(spec runtime.RunSpec) error {
	r.runs = append(r.runs, spec)
	return nil
}

func (r *recordingRuntime) Replace(spec runtime.ExecSpec) error {
	r.execs = append(r.execs, spec)
	return r.jsonRuntime.Replace(spec)
}

func (r *recordingRuntime) Attach(spec runtime.ExecSpec) (int, error) {
	r.execs = append(r.execs, spec)
	return r.jsonRuntime.Attach(spec)
}

// specText is what a boot and its exec hand the runtime, as stable text: the
// writers are left out, and the test's own directories are named by role so
// the text is the same on every machine.
func specText(t *testing.T, rt *recordingRuntime, dirs map[string]string) string {
	t.Helper()
	type view struct {
		Run  []runtime.RunSpec
		Exec []runtime.ExecSpec
	}
	runs := make([]runtime.RunSpec, len(rt.runs))
	for i, s := range rt.runs {
		s.Progress, s.Notice = nil, nil
		runs[i] = s
	}
	execs := make([]runtime.ExecSpec, len(rt.execs))
	for i, s := range rt.execs {
		s.Stdin, s.Ctx = nil, nil
		execs[i] = s
	}
	blob, err := json.MarshalIndent(view{Run: runs, Exec: execs}, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	text := string(blob)
	for role, dir := range dirs {
		text = strings.ReplaceAll(text, dir, "<"+role+">")
	}
	return text
}

// A run of a profile hands the runtime the same boot and the same exec, and
// says the same things, as it did before a run could read a Brigfile. The
// profile has a policy attached, a project and a published port, so the
// egress rules, the shares and the ports are all in what is pinned.
func TestARunOfAProfileHandsTheRuntimeWhatItDid(t *testing.T) {
	rt := &recordingRuntime{}
	jsonRunHost(t, &rt.jsonRuntime)
	swap(t, &detectRuntimeFor, func(runtime.Preference) (runtime.Runtime, error) { return rt, nil })
	pol := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", pol)
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	t.Setenv("BRIG_GIT_IDENTITY", "0")
	t.Setenv("GIT_TERMINAL_PROMPT", "")
	writePolicyFile(t, pol, "no-net", noNetBody)
	attach(t, "no-net", "faker")
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)

	out, stderr, err := runQuiet(t, "run", "faker", "--publish", "8080", ".", "--", "--flag", "word")
	if err != nil {
		t.Fatalf("run faker: %v\n%s", err, stderr)
	}
	dirs := map[string]string{"state": os.Getenv("BRIG_STATE_DIR"), "home": os.Getenv("BRIG_WORKSPACE"),
		"project": project}
	got := specText(t, rt, dirs)
	const want = `{
 "Run": [
  {
   "Name": "brig-faker",
   "Image": "img",
   "Digest": "",
   "Pull": "missing",
   "Net": "isolated",
   "Mem": 1024,
   "CPUs": 1,
   "Shares": [
    {
     "Host": "<home>",
     "Guest": "/home/faker",
     "ReadOnly": false
    },
    {
     "Host": "<project>",
     "Guest": "/work/PROJECT_BASE",
     "ReadOnly": false
    }
   ],
   "Env": [
    {
     "Name": "HOME",
     "Value": "/home/faker",
     "Secret": false
    },
    {
     "Name": "GIT_TERMINAL_PROMPT",
     "Value": "0",
     "Secret": false
    },
    {
     "Name": "BRIG_PROJECT",
     "Value": "/work/PROJECT_BASE",
     "Secret": false
    }
   ],
   "GUI": false,
   "GUITitle": "",
   "Hypervisor": "",
   "RootfsType": "",
   "RootfsMode": "",
   "Tmpfs": null,
   "GenericBoot": false,
   "BootAssets": {
    "Kernel": "",
    "Initrd": "",
    "Named": false
   },
   "Egress": {
    "Default": "deny",
    "Allow": [
     {
      "Host": "api.anthropic.com",
      "CIDR": ""
     }
    ],
    "Deny": null,
    "Divert": {
     "Routes": null,
     "Socket": "",
     "BrigInternal": false
    }
   },
   "Setup": null,
   "Publish": [
    {
     "hostPort": 8080,
     "guestPort": 8080
    }
   ],
   "Counted": true,
   "Progress": null,
   "Notice": null
  }
 ],
 "Exec": [
  {
   "Name": "brig-faker",
   "Cmd": [
    "agent",
    "--flag",
    "word"
   ],
   "Cwd": "/work/PROJECT_BASE",
   "TTY": false,
   "Env": [
    {
     "Name": "HOME",
     "Value": "/home/faker",
     "Secret": false
    },
    {
     "Name": "GIT_TERMINAL_PROMPT",
     "Value": "0",
     "Secret": false
    },
    {
     "Name": "BRIG_PROJECT",
     "Value": "/work/PROJECT_BASE",
     "Secret": false
    }
   ],
   "Counted": true,
   "Stdin": null,
   "User": "",
   "Ctx": null
  }
 ]
}`
	if wantNow := strings.ReplaceAll(want, "PROJECT_BASE", filepath.Base(project)); got != wantNow {
		t.Errorf("a run of a profile hands the runtime something else now:\n%s\nwant:\n%s", got, wantNow)
	}
	if out != "" {
		t.Errorf("a run of a profile printed %q on stdout", out)
	}
	for role, dir := range dirs {
		stderr = strings.ReplaceAll(stderr, dir, "<"+role+">")
	}
	if stderr != "brig: BRIG_VERIFY=off, so the guest image is not checked before it boots\n" {
		t.Errorf("a run of a profile says something else now:\n%q", stderr)
	}
}

// The lines that name no profile and no target behave as they did on a host
// with no Brigfile: the same message and the same exit, and nothing asked.
func TestARunWithNoProfileAndNoBrigfileIsUnchanged(t *testing.T) {
	for _, c := range []struct {
		line []string
		exit int
		msg  string
	}{
		{[]string{"run"}, 1, "run needs a profile, for example `brig run claude`. `brig agent ls` lists them"},
		{[]string{"run", "-d"}, 1, "run needs a profile, for example `brig run claude`. `brig agent ls` lists them"},
		{[]string{"run", "--image", "x"}, 1, "run needs a profile, for example `brig run claude`. `brig agent ls` lists them"},
		{[]string{"run", "--", "foo"}, 1, "run needs a profile, for example `brig run claude`. `brig agent ls` lists them"},
		{[]string{"run", "-f", "x", "claude"}, 2, `unknown flag "-f" before the profile name. brig's own flags ` +
			`come before the profile and the agent's after it; put "-f" after the profile to pass it through, ` +
			`or -- to end brig's flags`},
	} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		scratchHost(t)
		t.Chdir(dir)
		_, stderr, err := runQuiet(t, c.line...)
		if got := exitCode(err); got != c.exit || err == nil || err.Error() != c.msg {
			t.Errorf("brig %s: exit %d, %v; want exit %d, %s", strings.Join(c.line, " "), got, err, c.exit, c.msg)
		}
		if stderr != "" {
			t.Errorf("brig %s said %q", strings.Join(c.line, " "), stderr)
		}
	}
}
