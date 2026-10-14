package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BRIG_HULL_STORE_DIR goes in front of every hull verb that opens the store,
// and nowhere when unset.
func TestHullArgsNameTheStoreBeforeTheVerb(t *testing.T) {
	t.Setenv(HullStoreDir, "")
	if got := strings.Join(hullArgs("ps", "-a"), " "); got != "ps -a" {
		t.Errorf("unset: %q", got)
	}
	t.Setenv(HullStoreDir, "/st/b")
	dir := t.TempDir()
	bin, log := filepath.Join(dir, "hull"), filepath.Join(dir, "argv.log")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$*\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &hull{bin: bin}
	_, _ = h.Running("s")
	_, _ = h.Migration("s")
	_ = h.Stop("s")
	argv, _, err := h.replaceCmd(ExecSpec{Name: "s", Cmd: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(argv[1:4], " ") != "--store-dir /st/b exec" {
		t.Errorf("handover argv %q", argv)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(got)), "\n") {
		if !strings.HasPrefix(line, "--store-dir /st/b ") {
			t.Errorf("hull was asked %q without the store", line)
		}
	}
}

// A hull line printed as a remedy names the store the way the calls do, so
// it finds the sandbox when it is copied, and is quoted for a shell.
func TestHullLineNamesTheStore(t *testing.T) {
	t.Setenv(HullStoreDir, "")
	if got := HullLine("migrate", "--status", "brig-x"); got != "hull migrate --status brig-x" {
		t.Errorf("unset: %q", got)
	}
	t.Setenv(HullStoreDir, "/st/my store")
	if got := HullLine("restore", "brig-x"); got != "hull --store-dir '/st/my store' restore brig-x" {
		t.Errorf("set: %q", got)
	}
}

// The remedy for a missing portable kernel installs it, with hull's record of
// it, in the store brig drives: hull boots the profile only with that record,
// and a kernel pulled into hull's default store leaves the next run refused
// the same way. hull needs the local file, as nothing publishes the kernel.
func TestPortableKernelRemedyNamesTheStore(t *testing.T) {
	t.Setenv(HullStoreDir, "/st/b4 store")
	want := "HULL_PORTABLE_KERNEL=PATH hull --store-dir '/st/b4 store' assets pull --profile " + MachinePortableArm64V1
	if got := PortableKernelPullLine(MachinePortableArm64V1); got != want {
		t.Errorf("the pull line is %q, want %q", got, want)
	}
	err := &PortableKernelMissingError{Path: "/a/portable-arm64-v1/Image"}
	if !strings.Contains(err.Error(), "`"+want+"`") {
		t.Errorf("the missing-kernel refusal names no install into the store: %v", err)
	}
}
