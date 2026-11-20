package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const pipedPolicy = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata:
  name: gh-demo
spec:
  egress:
    default: deny
`

// stdinFrom makes os.Stdin read s, from a regular file as `<` gives it or
// from a pipe as `|` and a heredoc in some shells do.
func stdinFrom(t *testing.T, s string, pipe bool) {
	t.Helper()
	var f *os.File
	if pipe {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		go func() { w.WriteString(s); w.Close() }()
		f = r
	} else {
		path := filepath.Join(t.TempDir(), "stdin")
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		var err error
		if f, err = os.Open(path); err != nil {
			t.Fatal(err)
		}
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; f.Close() })
}

// leftovers lists what create left in the policy directory besides want.
func leftovers(t *testing.T, dir string, want ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !strings.Contains(strings.Join(want, " "), e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// A policy piped in is checked and written as it is. No editor opens.
func TestCreatePolicyReadsAPipedDocument(t *testing.T) {
	for _, pipe := range []bool{true, false} {
		dir := t.TempDir()
		t.Setenv("BRIG_POLICY_DIR", dir)
		stubEditor(t, `false`)
		stdinFrom(t, pipedPolicy, pipe)
		if err := createPolicy([]string{"gh-demo"}); err != nil {
			t.Fatalf("pipe=%v: %v", pipe, err)
		}
		blob, err := os.ReadFile(filepath.Join(dir, "gh-demo.yaml"))
		if err != nil || string(blob) != pipedPolicy {
			t.Errorf("pipe=%v: wrote %q, %v; want the piped document", pipe, blob, err)
		}
		if l := leftovers(t, dir, "gh-demo.yaml"); len(l) != 0 {
			t.Errorf("pipe=%v: left %v", pipe, l)
		}
	}
}

// A piped document that does not parse, is named otherwise, or is empty is
// refused, and nothing is written.
func TestCreatePolicyRefusesABadPipedDocument(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"invalid", "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: gh-demo}\nspec: {egres: {}}\n", "was not written"},
		{"another name", strings.Replace(pipedPolicy, "name: gh-demo", "name: other", 1), `was not written: the document is named "other", not "gh-demo"`},
		{"empty", "", "stdin is empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("BRIG_POLICY_DIR", dir)
			stubEditor(t, `false`)
			stdinFrom(t, c.in, true)
			err := createPolicy([]string{"gh-demo"})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if l := leftovers(t, dir); len(l) != 0 {
				t.Errorf("left %v", l)
			}
		})
	}
}

// An existing policy is replaced from stdin only with --force.
func TestCreatePolicyFromStdinKeepsTheForceRule(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_POLICY_DIR", dir)
	stubEditor(t, `false`)
	path := filepath.Join(dir, "gh-demo.yaml")
	old := strings.Replace(pipedPolicy, "default: deny", "default: allow", 1)
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	stdinFrom(t, pipedPolicy, true)
	if err := createPolicy([]string{"gh-demo"}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v, want a refusal naming --force", err)
	}
	if blob, _ := os.ReadFile(path); string(blob) != old {
		t.Fatalf("the policy changed without --force:\n%s", blob)
	}
	stdinFrom(t, pipedPolicy, true)
	if err := createPolicy([]string{"gh-demo", "--force"}); err != nil {
		t.Fatal(err)
	}
	if blob, _ := os.ReadFile(path); string(blob) != pipedPolicy {
		t.Fatalf("--force did not replace it:\n%s", blob)
	}
}

// A socket on stdin is not a document: create opens the editor, as
// docs/cli.md says.
func TestStdinPipedIsFalseForASocket(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fds[0]), "socket")
	t.Cleanup(func() { syscall.Close(fds[1]) })
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; f.Close() })
	if stdinPiped() {
		t.Error("stdinPiped() = true for a socket, want false")
	}
}
