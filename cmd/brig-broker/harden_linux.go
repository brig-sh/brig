//go:build linux && (amd64 || arm64)

package main

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// deniedSyscalls are the system calls a broker for one sandbox never makes,
// which hand a process that took the broker over a way out of its uid: other
// processes' memory, namespaces, mounts, the kernel's module, BPF and
// keyring interfaces, and io_uring, whose operations a filter does not see.
// Each fails with EPERM.
var deniedSyscalls = []uintptr{
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV,
	unix.SYS_UNSHARE, unix.SYS_SETNS,
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT,
	unix.SYS_FSOPEN, unix.SYS_FSCONFIG, unix.SYS_FSMOUNT, unix.SYS_FSPICK,
	unix.SYS_MOVE_MOUNT, unix.SYS_OPEN_TREE,
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
	unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
	unix.SYS_KEXEC_LOAD, unix.SYS_REBOOT, unix.SYS_SWAPON, unix.SYS_SWAPOFF,
	unix.SYS_ACCT, unix.SYS_QUOTACTL, unix.SYS_SYSLOG, unix.SYS_PERSONALITY,
	unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT,
	unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
}

// cloneNamespaces are the flags of clone that make a namespace.
const cloneNamespaces = unix.CLONE_NEWNS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC |
	unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET

// The offsets in struct seccomp_data.
const (
	offNr   = 0
	offArch = 4
	offArg0 = 16
)

// hardenSandbox sets no_new_privs and installs a seccomp filter on every
// thread. The filter kills the process on a foreign architecture, refuses
// deniedSyscalls and clone3 (whose flags it cannot read) and a clone that
// makes a namespace, and allows everything else.
func hardenSandbox() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	prog := seccompFilter()
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	// TSYNC puts the filter on every thread the Go runtime already started.
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&fprog))); errno != 0 {
		return fmt.Errorf("seccomp filter: %w", errno)
	}
	runtime.KeepAlive(prog)
	return nil
}

func stmt(code uint16, k uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: k} }

func jump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

func seccompFilter() []unix.SockFilter {
	const (
		ld    = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq   = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jge   = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
		jset  = unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K
		ret   = unix.BPF_RET | unix.BPF_K
		allow = unix.SECCOMP_RET_ALLOW
		kill  = unix.SECCOMP_RET_KILL_PROCESS
	)
	deny := uint32(unix.SECCOMP_RET_ERRNO | (uint32(unix.EPERM) & unix.SECCOMP_RET_DATA))
	nosys := uint32(unix.SECCOMP_RET_ERRNO | (uint32(unix.ENOSYS) & unix.SECCOMP_RET_DATA))
	p := []unix.SockFilter{
		stmt(ld, offArch),
		jump(jeq, auditArch, 1, 0),
		stmt(ret, kill),
		stmt(ld, offNr),
	}
	if runtime.GOARCH == "amd64" {
		// The x32 ABI of amd64 numbers its calls from bit 30 up and would
		// reach every denied call under another number.
		p = append(p, jump(jge, 0x40000000, 0, 1), stmt(ret, deny))
	}
	for _, nr := range deniedSyscalls {
		p = append(p, jump(jeq, uint32(nr), 0, 1), stmt(ret, deny))
	}
	p = append(p,
		jump(jeq, uint32(unix.SYS_CLONE3), 0, 1), stmt(ret, nosys),
		jump(jeq, uint32(unix.SYS_CLONE), 0, 3),
		stmt(ld, offArg0),
		jump(jset, cloneNamespaces, 0, 1), stmt(ret, deny),
		stmt(ret, allow),
	)
	return p
}
