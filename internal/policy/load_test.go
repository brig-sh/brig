package policy

import (
	"os"
	"path/filepath"
	"testing"
)

func writePolicy(t *testing.T, dir, filename, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A fresh install has no policy directory. That is not an error: it just
// means there are no policies yet.
func TestLoadAllIgnoresAMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	entries, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %v, want none", entries)
	}
}

// The registered name comes from name: inside the file, not the filename --
// so a file need not be named after the policy it declares.
func TestLoadAllNameWinsOverFilename(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "whatever.yaml", "apiVersion: "+APIVersion+"\n"+
		"name: no-net\negress:\n  default: deny\n")

	entries, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	e, ok := entries["no-net"]
	if !ok {
		t.Fatalf("entries = %v, want a %q entry", entries, "no-net")
	}
	if e.Path != filepath.Join(dir, "whatever.yaml") {
		t.Errorf("Path = %q, want %q", e.Path, filepath.Join(dir, "whatever.yaml"))
	}
}

// One bad file does not stop the others from loading.
func TestLoadAllSkipsAFileThatFailsToParse(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "good.yaml", "apiVersion: "+APIVersion+"\n"+
		"name: no-net\negress:\n  default: deny\n")
	writePolicy(t, dir, "bad.yaml", "apiVersion: "+APIVersion+"\n"+
		"name: broken\nunexpected: true\negress:\n  default: deny\n")

	entries, err := LoadAll(dir)
	if err == nil {
		t.Fatal("expected an error naming the broken file, got none")
	}
	if _, ok := entries["no-net"]; !ok {
		t.Errorf("the good policy did not load: %v", entries)
	}
	if _, ok := entries["broken"]; ok {
		t.Errorf("the broken policy loaded anyway: %v", entries)
	}
}

// Two files claiming the same name is reported, and the one that sorts last
// wins -- deterministic, and stated rather than merely determined.
func TestLoadAllReportsADuplicateName(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "a.yaml", "apiVersion: "+APIVersion+"\n"+
		"name: no-net\negress:\n  default: allow\n")
	writePolicy(t, dir, "b.yaml", "apiVersion: "+APIVersion+"\n"+
		"name: no-net\negress:\n  default: deny\n")

	entries, err := LoadAll(dir)
	if err == nil {
		t.Fatal("expected an error naming the duplicate, got none")
	}
	e, ok := entries["no-net"]
	if !ok {
		t.Fatalf("no-net is missing entirely: %v", entries)
	}
	if e.Path != filepath.Join(dir, "b.yaml") {
		t.Errorf("winner = %q, want b.yaml (sorts last)", e.Path)
	}
}

// Anything that is not a .yaml/.yml/.json file is not a policy, so it is
// ignored rather than reported as broken.
func TestLoadAllIgnoresNonPolicyFiles(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "README.md", "not a policy\n")
	writePolicy(t, dir, "no-net.yaml", "apiVersion: "+APIVersion+"\n"+
		"name: no-net\negress:\n  default: deny\n")

	entries, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("entries = %v, want exactly one", entries)
	}
}

// attachments.yaml lives in the same directory LoadAll scans, so it has to
// be ignored the same way a README is, not reported as a broken policy.
func TestLoadAllIgnoresTheAttachmentsFile(t *testing.T) {
	dir := t.TempDir()
	var a Attachments
	a.AttachToProfile("no-net", "claude-code")
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	writePolicy(t, dir, "no-net.yaml", "apiVersion: "+APIVersion+"\n"+
		"name: no-net\negress:\n  default: deny\n")

	entries, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll reported attachments.yaml as broken: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("entries = %v, want exactly one", entries)
	}
}
