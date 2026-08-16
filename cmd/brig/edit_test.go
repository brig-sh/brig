package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/profile"
)

// A stub editor, so the test drives the real code path without a real editor.
// It appends a line to whatever file it is handed.
func stubEditor(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-editor")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", path)
}

func TestEditOpensAFileBackedProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	blob := []byte("name: mine\nimage: i\nguestHome: /home/mine\nbinary: m\nmem: 1\ncpus: 1\n")
	path := filepath.Join(dir, "mine.yaml")
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	stubEditor(t, `printf 'cpus: 2\n' >> "$1" && sed -i.bak '/^cpus: 1$/d' "$1"`)

	if err := editProfile([]string{"mine"}); err != nil {
		t.Fatalf("editing a file-backed profile failed: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "cpus: 2") {
		t.Errorf("the editor's changes are not on disk:\n%s", after)
	}
}

// An embedded profile has no file. edit creates nothing -- import and export
// with a destination stay the only two commands that write into the profile
// directory -- and it names the command that would make one.
func TestEditRefusesAnEmbeddedProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	stubEditor(t, `echo should not run >&2; exit 1`)

	err := editProfile([]string{"claude-code"})
	if err == nil {
		t.Fatal("editing a built-in was allowed")
	}
	if !strings.Contains(err.Error(), "built in") ||
		!strings.Contains(err.Error(), "brig profile export claude-code") {
		t.Errorf("the error does not say how to make a file: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("edit wrote into the profile directory: %v", entries)
	}
}

// An unknown name is a different problem from a built-in one, and calls for a
// different next action.
func TestEditUnknownProfile(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	err := editProfile([]string{"nope"})
	if err == nil {
		t.Fatal("an unknown profile was accepted")
	}
	if !strings.Contains(err.Error(), "unknown profile") {
		t.Errorf("wrong error for an unknown name: %v", err)
	}
}

// brig never discards someone's edits to keep its own state tidy. A file saved
// unparseable is reported and left exactly as saved.
func TestEditReportsAnUnparseableSaveAndKeepsIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIG_PROFILE_DIR", dir)
	path := filepath.Join(dir, "mine.yaml")
	good := []byte("name: mine\nimage: i\nguestHome: /home/mine\nbinary: m\nmem: 1\ncpus: 1\n")
	if err := os.WriteFile(path, good, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	stubEditor(t, `printf 'name: [unclosed\n' > "$1"`)

	err := editProfile([]string{"mine"})
	if err == nil {
		t.Fatal("a broken save was accepted")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("the file was removed: %v", readErr)
	}
	if !strings.Contains(string(after), "unclosed") {
		t.Errorf("the edits were discarded:\n%s", after)
	}
}

// Aliases resolve first, so `brig profile edit claude` reaches claude-code --
// and then reports it as built in, because that is what it is.
func TestEditResolvesAliases(t *testing.T) {
	t.Setenv("BRIG_PROFILE_DIR", t.TempDir())
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	stubEditor(t, `exit 1`)
	err := editProfile([]string{"claude"})
	if err == nil || !strings.Contains(err.Error(), "claude-code") {
		t.Errorf("the alias did not resolve: %v", err)
	}
}

func TestEditorCommandPrecedence(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	if got := editorCommand(); got[0] != "vi" {
		t.Errorf("no editor set: got %v, want vi", got)
	}
	t.Setenv("EDITOR", "nano")
	if got := editorCommand(); got[0] != "nano" {
		t.Errorf("EDITOR ignored: %v", got)
	}
	t.Setenv("VISUAL", "code -w")
	got := editorCommand()
	if len(got) != 2 || got[0] != "code" || got[1] != "-w" {
		t.Errorf("VISUAL does not win, or its arguments were lost: %v", got)
	}
}
