package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// validBody is a profile that passes every rule, for a test to change.
const validBody = "name: mine\nimage: example.com/x:1\nguestHome: /home/mine\n" +
	"binary: mine\nmem: 512\ncpus: 1\n"

// validateFile writes body to a file of its own and returns its path.
func validateFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mine.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// validate runs `brig agent validate args...` through run(), the way the
// binary does, with a profile directory of its own.
func validate(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	return captureStdout(t, func() error {
		return run(append([]string{"agent", "validate"}, args...))
	})
}

// stdinWith gives a test a stdin that holds data.
func stdinWith(t *testing.T, data string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	w.Close()
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig; r.Close() })
}

// A file brig would take is valid, and the answer says which schema it was read
// by, and whether the file said so or brig supplied it. An author whose file
// names none should see that brig read it as 1, not that it found a 1.
func TestAgentValidateAGoodFileIsValid(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"no schemaVersion": {validBody, "schemaVersion 1 (the file names none)"},
		"schemaVersion 1":  {"schemaVersion: 1\n" + validBody, "schemaVersion 1"},
	} {
		t.Run(name, func(t *testing.T) {
			path := validateFile(t, tc.body)
			out, err := validate(t, path)
			if err != nil {
				t.Fatalf("validate = %v", err)
			}
			if !strings.Contains(out, path+": valid: agent mine, ") || !strings.Contains(out, tc.want) {
				t.Errorf("the answer does not say valid and %q:\n%s", tc.want, out)
			}
			if name == "schemaVersion 1" && strings.Contains(out, "names none") {
				t.Errorf("a file that names its version is told it names none:\n%s", out)
			}
		})
	}
}

// Each way a file can be wrong has its exit class, and the table pins them: a
// CI job branches on these, so they are the contract and not an accident.
// 1 is an invalid file, which is what every invalid document is today; 2 a
// mistyped command line; 3 a file or agent that is not there.
func TestAgentValidateExitClasses(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"a file brig refuses", []string{validateFile(t, validBody+"bogus: true\n")}, 1, `unknown field "bogus"`},
		{"a file for a newer brig", []string{validateFile(t, "schemaVersion: 2\nfuture: true\n"+validBody)}, 1, "schemaVersion 2 is not supported"},
		{"a file that fails a rule", []string{validateFile(t, strings.Replace(validBody, "mem: 512", "mem: 0", 1))}, 1, "mem and cpus"},
		{"a name import would refuse", []string{validateFile(t, strings.Replace(validBody, "name: mine", "name: desktop", 1))}, 1, "collides"},
		{"a file that is not there", []string{missing}, 3, "no file"},
		{"an agent that is not there", []string{"no-such-agent"}, 3, "no agent named"},
		{"nothing to check", nil, 2, "validate needs a file or an agent"},
		{"two things to check", []string{"claude-code", "codex"}, 2, "unexpected argument"},
		{"a flag that does not exist", []string{"--bogus", "claude-code"}, 2, "unknown flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validate(t, tc.args...)
			if err == nil {
				t.Fatal("accepted")
			}
			if got := exitCode(err); got != tc.code {
				t.Errorf("exit %d, want %d: %v", got, tc.code, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not say %q: %v", tc.want, err)
			}
		})
	}
}

// An invalid file's error names the file, so a CI log that checks several says
// which one failed.
func TestAgentValidateAnErrorNamesTheFile(t *testing.T) {
	path := validateFile(t, validBody+"bogus: true\n")
	_, err := validate(t, path)
	if err == nil || !strings.HasPrefix(err.Error(), path+": ") {
		t.Fatalf("validate = %v, want the error to start with the path", err)
	}
}

// What the word is depends on how it is spelled and not on what happens to
// exist: a name that is not an agent is not looked for as a file, and says how
// to ask for the file when one of that name is sitting there.
func TestAgentValidateAWordIsAFileOrANameBySpelling(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("mine", []byte(validBody), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := validate(t, "mine")
	if err == nil || exitCode(err) != 3 || !strings.Contains(err.Error(), "./mine") {
		t.Fatalf("validate mine = %v (exit %d): a bare word is a name, and the file is offered as ./mine", err, exitCode(err))
	}
	if out, err := validate(t, "./mine"); err != nil || !strings.Contains(out, "valid") {
		t.Errorf("validate ./mine = %q, %v", out, err)
	}
	if err := os.WriteFile("mine.json", []byte(`{"name":"mine","image":"example.com/x:1","guestHome":"/home/mine","binary":"mine","mem":512,"cpus":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := validate(t, "mine.json"); err != nil || !strings.Contains(out, "valid") {
		t.Errorf("validate mine.json = %q, %v: a .json suffix is a file", out, err)
	}
}

// - is stdin, and is reported as such.
func TestAgentValidateReadsStdin(t *testing.T) {
	stdinWith(t, validBody)
	out, err := validate(t, "-")
	if err != nil || !strings.Contains(out, "<stdin>: valid") {
		t.Fatalf("validate - = %q, %v", out, err)
	}
	stdinWith(t, validBody+"bogus: true\n")
	_, err = validate(t, "-")
	if err == nil || exitCode(err) != 1 || !strings.HasPrefix(err.Error(), "<stdin>: ") {
		t.Fatalf("validate - on a bad file = %v (exit %d)", err, exitCode(err))
	}
}

// An agent brig has, shipped or your own, is checked by name, aliases included,
// and every shipped profile passes: the question "is this file good" has to
// have the answer yes about the files brig itself ships.
func TestAgentValidateTheShippedProfilesAreValid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "mine.yaml"), []byte(validBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(dir); err != nil {
		t.Fatal(err)
	}
	var shipped []string
	for _, p := range profile.All() {
		if profile.Embedded(p.Name) {
			shipped = append(shipped, p.Name)
		}
	}
	if len(shipped) < 9 {
		t.Fatalf("found %d shipped profiles, want at least 9: %v", len(shipped), shipped)
	}
	for _, name := range append(shipped, "mine", "claude") {
		t.Run(name, func(t *testing.T) {
			out, err := captureStdout(t, func() error {
				return run([]string{"agent", "validate", name})
			})
			if err != nil || !strings.Contains(out, ": valid: ") {
				t.Errorf("validate %s = %q, %v", name, out, err)
			}
		})
	}
}

// A file in the profile directory that brig skipped is not an agent, so asking
// for it by name is not found: the warning at startup names the file and the
// reason, and the report does not invent a second place to look.
func TestAgentValidateAFileBrigSkippedIsNotFoundByName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte(validBody+"bogus: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := captureStdout(t, func() error {
		return run([]string{"agent", "validate", "mine"})
	})
	if err == nil || exitCode(err) != 3 {
		t.Fatalf("validate mine = %v (exit %d), want not found", err, exitCode(err))
	}
}

// validation is the payload of an AgentValidation document.
type validationDoc struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Data       agentValidationJSON `json:"data"`
}

func decodeValidation(t *testing.T, out string) validationDoc {
	t.Helper()
	var doc validationDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	return doc
}

// With --json the report is the answer: it is on stdout for a good file and for
// a bad one, the exit status carries the verdict, and the error line is still
// there for a person. Both spellings of the flag, because a script may use
// either.
func TestAgentValidateJSONReportsBothVerdictsOnStdout(t *testing.T) {
	good := validateFile(t, validBody)
	bad := validateFile(t, validBody+"bogus: true\n")
	for _, form := range [][]string{{"--json"}, nil} {
		spell := func(path string) []string {
			if form == nil {
				return []string{"--json", "agent", "validate", path}
			}
			return []string{"agent", "validate", path, "--json"}
		}
		t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
		out, err := captureStdout(t, func() error { return run(spell(good)) })
		if err != nil {
			t.Fatalf("a good file: %v", err)
		}
		doc := decodeValidation(t, out)
		if doc.APIVersion != jsonAPIVersion || doc.Kind != "AgentValidation" ||
			!doc.Data.Valid || doc.Data.Name != "mine" || doc.Data.SchemaVersion != 1 || doc.Data.Source != good {
			t.Errorf("the report of a good file is wrong: %+v", doc)
		}
		if !strings.Contains(out, `"errors": []`) {
			t.Errorf("errors is not an empty array for a good file:\n%s", out)
		}

		out, err = captureStdout(t, func() error { return run(spell(bad)) })
		if err == nil || exitCode(err) != 1 {
			t.Fatalf("a bad file: %v (exit %d), want exit 1", err, exitCode(err))
		}
		doc = decodeValidation(t, out)
		if doc.Data.Valid || len(doc.Data.Errors) != 1 || !strings.Contains(doc.Data.Errors[0], `unknown field "bogus"`) {
			t.Errorf("the report of a bad file is wrong: %+v", doc)
		}
	}
}

// Not-found and usage mistakes have no report to make, so --json does not
// invent one: stdout stays empty and the error is the usual text.
func TestAgentValidateJSONHasNoReportWhenThereIsNothingToCheck(t *testing.T) {
	for _, args := range [][]string{{"--json", "no-such-agent"}, {"--json"}} {
		out, err := validate(t, args...)
		if err == nil || out != "" {
			t.Errorf("validate %v = %q, %v, want an error and empty stdout", args, out, err)
		}
	}
}

// The other agent subcommands still have no --json to the left of the verb: only
// ls and validate list or report. Widening the gate for validate must not have
// widened it for rm.
func TestGlobalJSONIsStillRefusedOnTheOtherAgentVerbs(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	for _, verb := range []string{"rm", "edit", "import", "new"} {
		err := run([]string{"--json", "agent", verb, "x"})
		if err == nil || exitCode(err) != 2 || !strings.Contains(err.Error(), "agent validate") {
			t.Errorf("--json agent %s = %v (exit %d), want a usage error naming the verbs that take it", verb, err, exitCode(err))
		}
	}
}

// The promise: the report carries no credential value. A profile may hold a
// literal in env: value:, which is the one place a pasted secret can sit in a
// file, and no rule that refuses such a file may echo it. Each case plants the
// same string and makes the file invalid in a different way; the string must be
// in neither the error nor the JSON.
func TestAgentValidateNeverEchoesAnEnvLiteral(t *testing.T) {
	const planted = "sk-planted-7f3a9c"
	for name, env := range map[string]string{
		"a value and a ref":           "env: [{name: TOKEN, value: " + planted + ", ref: env.TOKEN}]\n",
		"a value and refs":            "env: [{name: TOKEN, value: " + planted + ", refs: [env.A, env.B]}]\n",
		"a value under a denied name": "deny: [TOKEN]\nenv: [{name: TOKEN, value: " + planted + "}]\n",
		"a name with a space":         "env: [{name: 'BAD NAME', value: " + planted + "}]\n",
		"a name with an equals":       "env: [{name: 'BAD=NAME', value: " + planted + "}]\n",
		"the same name twice":         "env: [{name: A, value: " + planted + "}, {name: A, value: " + planted + "}]\n",
		"a field brig lacks":          "env: [{name: A, value: " + planted + ", bogus: 1}]\n",
		"a value of the wrong type":   "env: [{name: A, value: [" + planted + "]}]\n",
		"a ref to nothing declared":   "env: [{name: A, value: " + planted + ", ref: secrets.nope}]\n",
		"a literal in a bad file":     "mem: 0\nenv: [{name: A, value: " + planted + "}]\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := validateFile(t, validBody+env)
			for _, args := range [][]string{{path}, {"--json", path}} {
				out, err := validate(t, args...)
				if err == nil {
					t.Fatalf("validate %v accepted a file this test means to be invalid", args)
				}
				if strings.Contains(out, planted) || strings.Contains(err.Error(), planted) {
					t.Errorf("the planted literal came back (%v):\nstdout: %s\nerror: %v", args, out, err)
				}
			}
		})
	}
}

// The group's help and error lists name the new verb, so a reader who mistypes
// one is told it exists.
func TestAgentUsageAndErrorsNameValidate(t *testing.T) {
	if !strings.Contains(agentUsage, "brig agent validate <file|agent>") {
		t.Error("the agent usage does not teach validate")
	}
	if !strings.Contains(usage, "brig agent validate") {
		t.Error("the global usage does not teach validate")
	}
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	for _, args := range [][]string{{"agent"}, {"agent", "nonsense"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "validate") {
			t.Errorf("brig %s = %v, want it to name validate", strings.Join(args, " "), err)
		}
	}
	out, err := captureStdout(t, func() error { return run([]string{"agent", "validate", "--help"}) })
	if err != nil || !strings.Contains(out, "brig agent validate") {
		t.Errorf("validate --help = %q, %v, want the group's usage and success", out, err)
	}
}
