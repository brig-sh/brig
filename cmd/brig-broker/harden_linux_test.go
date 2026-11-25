//go:build linux && (amd64 || arm64)

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const hardenHelperEnv = "BRIG_TEST_HARDEN_HELPER"

// TestHardenHelper runs in a child of TestHardenSandboxRefusesTheWayOut,
// since the filter cannot be taken off the process it is put on.
func TestHardenHelper(t *testing.T) {
	if os.Getenv(hardenHelperEnv) != "1" {
		return
	}
	if err := hardenSandbox(); err != nil {
		fmt.Println("harden:", err)
		os.Exit(1)
	}
	check := func(name string, err error, want error) {
		if !errors.Is(err, want) {
			fmt.Printf("%s: got %v, want %v\n", name, err, want)
			os.Exit(1)
		}
	}
	check("unshare", unix.Unshare(unix.CLONE_NEWUSER), unix.EPERM)
	check("ptrace", unix.PtraceAttach(os.Getppid()), unix.EPERM)
	check("mount", unix.Mount("none", "/tmp", "tmpfs", 0, ""), unix.EPERM)
	check("bpf", errnoOf(unix.Syscall(unix.SYS_BPF, 0, 0, 0)), unix.EPERM)
	check("io_uring", errnoOf(unix.Syscall(unix.SYS_IO_URING_SETUP, 1, 0, 0)), unix.EPERM)
	check("keyctl", errnoOf(unix.Syscall(unix.SYS_KEYCTL, 0, 0, 0)), unix.EPERM)
	check("clone3", errnoOf(unix.Syscall(unix.SYS_CLONE3, 0, 0, 0)), unix.ENOSYS)
	check("clone newuser", errnoOf(unix.RawSyscall(unix.SYS_CLONE, unix.CLONE_NEWUSER, 0, 0)), unix.EPERM)
	if v, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0); err != nil || v != 1 {
		fmt.Println("no_new_privs is not set:", v, err)
		os.Exit(1)
	}
	// What a broker does still works: files, sockets and threads.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	done := make(chan error, 1)
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			_ = c.Close()
		}
		done <- err
	}()
	if c, err := ln.Accept(); err != nil {
		fmt.Println("accept:", err)
		os.Exit(1)
	} else {
		_ = c.Close()
	}
	if err := <-done; err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	if _, err := os.ReadFile("/proc/self/status"); err != nil {
		fmt.Println("read:", err)
		os.Exit(1)
	}
	fmt.Println("HARDENED")
	os.Exit(0)
}

func errnoOf(_, _ uintptr, errno unix.Errno) error {
	if errno == 0 {
		return nil
	}
	return errno
}

// A broker for one sandbox takes no new privileges and is refused the
// system calls that lead out of its uid, and still serves.
func TestHardenSandboxRefusesTheWayOut(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHardenHelper$")
	cmd.Env = append(os.Environ(), hardenHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "HARDENED") {
		t.Fatalf("the hardened helper failed: %v\n%s", err, out)
	}
}
