package main

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// acl builds system.posix_acl_access from tag, permission and id triples.
func acl(entries ...[3]uint32) []byte {
	b := binary.LittleEndian.AppendUint32(nil, 2)
	for _, e := range entries {
		b = binary.LittleEndian.AppendUint16(b, uint16(e[0]))
		b = binary.LittleEndian.AppendUint16(b, uint16(e[1]))
		b = binary.LittleEndian.AppendUint32(b, e[2])
	}
	return b
}

// The kvm group is the grant a rootless run loses. Owner, a named entry for
// this uid under the mask, and the bits for everyone are what it keeps.
func TestRootlessGrantLeavesGroupsOut(t *testing.T) {
	const me, other = 1000, 2000
	const user, group, mask = 0x02, 0x08, 0x10
	for _, tc := range []struct {
		name  string
		mode  os.FileMode
		owner uint32
		acl   []byte
		want  bool
	}{
		{"owner with rw", 0o600, me, nil, true},
		{"owner through the group bits only", 0o060, me, nil, false},
		{"group only, as udev leaves kvm", 0o660, 0, nil, false},
		{"rw for everyone", 0o666, 0, nil, true},
		{"a named entry", 0o660, 0, acl([3]uint32{user, 6, me}, [3]uint32{mask, 6, 0}), true},
		{"a named entry the mask cuts", 0o660, 0, acl([3]uint32{user, 6, me}, [3]uint32{mask, 4, 0}), false},
		{"a named entry without write, everyone with it", 0o666, 0, acl([3]uint32{user, 4, me}), false},
		{"another user's entry", 0o660, 0, acl([3]uint32{user, 6, other}, [3]uint32{mask, 6, 0}), false},
		{"a group entry", 0o660, 0, acl([3]uint32{group, 6, 36}, [3]uint32{mask, 6, 0}), false},
		{"an ACL of another version", 0o660, 0, append([]byte{1, 0, 0, 0}, acl([3]uint32{user, 6, me})[4:]...), false},
	} {
		if got := rootlessGrant(tc.mode, tc.owner, me, tc.acl); got != tc.want {
			t.Errorf("%s: rootlessGrant = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The device opens at the doctor prompt, and a run that drops the group still
// fails. The row says so.
func TestDevicesCheckRefusesAGroupOnlyDevice(t *testing.T) {
	dev := writeFile(t, "")
	if err := os.Chmod(dev, 0o660); err != nil {
		t.Fatal(err)
	}
	swap(t, &posixACLOf, func(string) []byte { return nil })
	// Not the owner: only the group bits are left, and the run drops them.
	swap(t, &geteuid, func() int { return os.Geteuid() + 1 })
	c := devicesCheck(dev)
	if c.State != stateFail || !strings.Contains(c.Finding, "only through a group") || !strings.Contains(c.Fix, "install.sh") {
		t.Errorf("a group-only device: %+v", c)
	}
	swap(t, &posixACLOf, func(string) []byte {
		return acl([3]uint32{0x02, 6, uint32(os.Geteuid() + 1)}, [3]uint32{0x10, 6, 0})
	})
	if c := devicesCheck(dev); c.State != statePass {
		t.Errorf("a device granted to this user by name: %+v", c)
	}
	// Root runs no user namespace.
	swap(t, &posixACLOf, func(string) []byte { return nil })
	swap(t, &geteuid, func() int { return 0 })
	if c := devicesCheck(dev); c.State != statePass {
		t.Errorf("root: %+v", c)
	}
}

// /dev/kvm opens through the kvm group at the prompt. The virtual line asks
// again for a rootless nerdctl run, and only for one.
func TestVirtualLineAsksWhatARootlessRunKeeps(t *testing.T) {
	swap(t, &hostOS, "linux")
	swap(t, &geteuid, func() int { return 1000 })
	asked := ""
	swap(t, &rootlessDeviceDenied, func(dev string) string {
		asked = dev
		return dev + " opens for this user only through a group"
	})
	pass := check{Name: "virtual", State: statePass, Finding: "/dev/kvm is present and openable"}
	c := rootlessKVM(pass, nerdctlDoctorRuntime{})
	if c.State != stateFail || asked != "/dev/kvm" || !strings.Contains(c.Fix, "install.sh") {
		t.Errorf("a group-only /dev/kvm under a rootless run: %+v", c)
	}
	if c := rootlessKVM(pass, nil); c.State != stateFail {
		t.Errorf("no runtime yet: %+v", c)
	}
	for name, rt := range map[string]runtime.Runtime{"hull": doctorRuntime{}} {
		if c := rootlessKVM(pass, rt); c.State != statePass {
			t.Errorf("%s: %+v", name, c)
		}
	}
	failed := check{Name: "virtual", State: stateFail, Finding: "/dev/kvm could not be opened"}
	if c := rootlessKVM(failed, nerdctlDoctorRuntime{}); c != failed {
		t.Errorf("a failed line was replaced: %+v", c)
	}
	swap(t, &geteuid, func() int { return 0 })
	if c := rootlessKVM(pass, nerdctlDoctorRuntime{}); c.State != statePass {
		t.Errorf("root: %+v", c)
	}
	swap(t, &geteuid, func() int { return 1000 })
	swap(t, &hostOS, "darwin")
	if c := rootlessKVM(pass, nerdctlDoctorRuntime{}); c.State != statePass {
		t.Errorf("macOS: %+v", c)
	}
}
