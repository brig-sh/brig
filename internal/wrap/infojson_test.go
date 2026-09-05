package wrap

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
)

// InfoData is the Info payload, and the one thing it must never do is carry a
// value. #7 calls this the defect if it fails: a machine-readable dump is one
// more place a credential can leak, and this asserts it does not -- from the
// resolved store values a run holds and from the reporting names, the two
// places a value could get in.
func TestInfoDataNamesCredentialsNeverValues(t *testing.T) {
	// A profile that delivers a stored secret as a file, so the file half of the
	// credential list has something to name.
	c := bindingConfig(t, "secrets:\n  - tok\n"+
		"volumes:\n  - kind: tmpfs\n    path: .config\n"+
		"files:\n  - ref: secrets.tok\n    path: .config/cred\n    mode: \"0600\"\n")
	c.Runtime = nil // as `brig info` runs it with no runtime on PATH

	const planted = "PLANTED-TOKEN-VALUE"
	// The value a run resolved, kept on the Config exactly as BuildEnv leaves it
	// for the file-delivery step to read.
	c.secrets = creds.Resolution{Values: map[string]string{"tok": planted}}
	// And an environment-sourced credential, annotated the way creds.Set does.
	set := creds.Set{Names: []string{"CLAUDE_TOKEN(secret)"}}

	d := c.InfoData(set)

	blob, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), planted) {
		t.Fatalf("info --json leaked a credential value:\n%s", blob)
	}

	// The names and sources are there, which is the point of the row: the value
	// is gone, the fact that there is one is not.
	var gotEnv, gotFile bool
	for _, cr := range d.Credentials {
		switch cr.Name {
		case "CLAUDE_TOKEN":
			gotEnv = cr.Source == "secret"
		case "tok":
			gotFile = cr.Source == "file"
		}
	}
	if !gotEnv {
		t.Errorf("the env credential is not named with its source: %+v", d.Credentials)
	}
	if !gotFile {
		t.Errorf("the file credential is not named with its source: %+v", d.Credentials)
	}
}

// With no runtime on PATH, `brig info --json` still prints a complete object and
// exits 0 -- ErrNoRuntime is swallowed for info, and --json must not undo that.
// The runtime is marked unavailable rather than the object being cut short.
func TestInfoDataWithoutRuntimeIsComplete(t *testing.T) {
	c := bindingConfig(t, "")
	c.Runtime = nil

	d := c.InfoData(creds.Set{})

	if d.Runtime.Available {
		t.Error("the runtime is marked available with none on PATH")
	}
	if d.Runtime.Kind != "" || d.Runtime.Bin != "" {
		t.Errorf("an unavailable runtime still names a kind or bin: %+v", d.Runtime)
	}
	// The rest of the object is there: a report that dropped its own fields
	// because one line could not be filled is the failure this guards.
	for name, got := range map[string]string{
		"profile":   d.Profile,
		"sandbox":   d.Sandbox,
		"workspace": d.Workspace,
		"isolation": d.Isolation,
		"network":   d.Network,
	} {
		if got == "" {
			t.Errorf("info --json without a runtime left %s empty", name)
		}
	}
}
