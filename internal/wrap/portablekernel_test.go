package wrap

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/verify"
)

// The portable kernel a test writes, and one byte off it.
var (
	pinnedPortable   = []byte("the portable kernel brig pins")
	modifiedPortable = []byte("the portable kernel brig pins!")
)

// pinPortable pins the portable kernel the tests write, in place of the
// build brig ships.
func pinPortable(t *testing.T) {
	t.Helper()
	was := portableKernelDigest
	portableKernelDigest = digestOf(pinnedPortable)
	t.Cleanup(func() { portableKernelDigest = was })
}

// The bundle lists no portable kernel, so the bundle's check compares the
// initrd alone and the pin decides the kernel. A kernel that matches the pin
// beside an initrd that matches the bundle boots, and only then does the
// summary name both.
func TestThePortableKernelIsCheckedAgainstThePinAndTheInitrdAgainstTheBundle(t *testing.T) {
	registryServes(t, published())
	pinPortable(t)
	for _, mode := range []verify.Mode{verify.Warn, verify.Require} {
		for _, named := range []bool{false, true} {
			c, rr := digestRun(t, mode, writePortableAssets(t, pinnedPortable, named))

			// The stand-in runtime refuses the boot itself; reaching it is
			// what counts.
			_ = c.EnsureRunning(creds.Set{})

			reachedRun(t, string(mode), rr)
			said := c.Err.(*bytes.Buffer).String()
			if !strings.Contains(said, "image and boot assets and portable kernel verified") {
				t.Errorf("%s, named %v: the summary does not name the portable kernel:\n%s", mode, named, said)
			}
		}
	}
}

// A portable kernel at the place brig looks by default that is not the pinned
// build has no innocent reading: refused under warn as under require. Named
// in BRIG_PORTABLE_KERNEL it is somebody's build: warn says so and boots,
// require refuses. off boots it unchecked.
func TestAPortableKernelThatIsNotThePinnedBuild(t *testing.T) {
	registryServes(t, published())
	pinPortable(t)
	cases := []struct {
		mode   verify.Mode
		named  bool
		refuse bool
	}{
		{verify.Warn, false, true},
		{verify.Require, false, true},
		{verify.Warn, true, false},
		{verify.Require, true, true},
	}
	for _, tc := range cases {
		c, rr := digestRun(t, tc.mode, writePortableAssets(t, modifiedPortable, tc.named))

		err := c.EnsureRunning(creds.Set{})

		said := c.Err.(*bytes.Buffer).String()
		if tc.refuse {
			refused(t, string(tc.mode), err, rr)
			if !strings.Contains(err.Error(), digestOf(pinnedPortable)) || !strings.Contains(err.Error(), "BRIG_") {
				t.Errorf("%s, named %v: the refusal names neither the pin nor a way forward: %v", tc.mode, tc.named, err)
			}
		} else {
			reachedRun(t, string(tc.mode), rr)
			if !strings.Contains(said, "not the one brig pins") {
				t.Errorf("%s, named %v: the boot did not say the kernel differs:\n%s", tc.mode, tc.named, said)
			}
		}
		if strings.Contains(said, "portable kernel verified") {
			t.Errorf("%s, named %v: the summary claimed the portable kernel verified:\n%s", tc.mode, tc.named, said)
		}
	}
}

// The pin is checked whatever the bundle's check found: here no signature
// verified for the bundle, so there is no bundle list to bind, and the kernel
// still meets the pin.
func TestThePortableKernelIsCheckedWithNoBundleToBind(t *testing.T) {
	pinPortable(t)
	c := testConfig(t, t.TempDir(), t.TempDir())
	c.Verify = verify.Warn
	c.Profile.GenericBoot = true

	if err := c.checkBootDigests(writePortableAssets(t, modifiedPortable, false)); err != nil {
		t.Fatalf("the bundle's check, with no bundle digest, refused: %v", err)
	}
	if err := c.checkPortableKernel(writePortableAssets(t, modifiedPortable, false)); err == nil {
		t.Fatal("a portable kernel that is not the pinned build booted where brig chose it")
	}
	if err := c.checkPortableKernel(writePortableAssets(t, pinnedPortable, false)); err != nil {
		t.Fatalf("the pinned build was refused: %v", err)
	}
	c.Verify = verify.Off
	if err := c.checkPortableKernel(writePortableAssets(t, modifiedPortable, false)); err != nil {
		t.Fatalf("BRIG_VERIFY=off compared the kernel anyway: %v", err)
	}
	// The bundle's own kernel is not this check's to judge.
	plain := writeBootAssets(t, modifiedPortable, publishedInitrd, false)
	c.Verify = verify.Require
	if err := c.checkPortableKernel(plain); err != nil {
		t.Fatalf("the native machine's kernel was held to the portable pin: %v", err)
	}
}

// A kernel brig cannot hash is "cannot check": said under warn, refused
// under require.
func TestAnUnreadablePortableKernelWarnsOrRefuses(t *testing.T) {
	pinPortable(t)
	gone := writePortableAssets(t, pinnedPortable, true)
	if err := os.Remove(gone.Kernel); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t, t.TempDir(), t.TempDir())
	c.Verify = verify.Warn
	if err := c.checkPortableKernel(gone); err != nil {
		t.Errorf("warn refused a kernel it could not read: %v", err)
	}
	if said := c.Err.(*bytes.Buffer).String(); !strings.Contains(said, "cannot read the portable kernel") {
		t.Errorf("warn did not say it could not compare the kernel:\n%s", said)
	}
	c.Verify = verify.Require
	if err := c.checkPortableKernel(gone); err == nil || !strings.Contains(err.Error(), "BRIG_VERIFY=require") {
		t.Errorf("require booted a kernel it could not read: %v", err)
	}
}
