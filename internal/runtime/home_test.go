package runtime

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/brig-sh/brig/internal/egress"
)

// Every boot, stop and removal through hull reaches the gateway directory: a
// boot that is not isolated stops the isolated gateway left under the
// sandbox's name, a stop or a removal releases it, and a failed boot withdraws
// the sandbox's publications. Unpinned, that directory is the ~/.brig of
// whoever runs the tests, and a stub booted as brig-x or vm cleared the
// records of a real sandbox of that name and signalled its gateway.
//
// TestMain is the backstop for a test that forgets scratchHome. It points HOME
// and BRIG_GATEWAY_DIR at a directory of its own, and fails the run if
// anything was written there: a test that writes into it would have written
// into the real ~/.brig without this.
func TestMain(m *testing.M) {
	// startResolver runs the resolver from the running executable, which is
	// this test binary under go test.
	if len(os.Args) > 1 && os.Args[1] == egress.Verb {
		os.Exit(egress.Main(os.Args[2:], os.Stderr))
	}
	home, err := os.MkdirTemp("", "brig-home-")
	if err != nil {
		panic(err)
	}
	for name, value := range map[string]string{
		"HOME":              home,
		"BRIG_GATEWAY_DIR":  filepath.Join(home, ".brig"),
		"BRIG_GATEWAY_SOCK": "",
	} {
		if err := os.Setenv(name, value); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	var reached []string
	_ = filepath.WalkDir(home, func(path string, _ fs.DirEntry, err error) error {
		if err == nil && path != home {
			reached = append(reached, path)
		}
		return nil
	})
	if len(reached) > 0 {
		fmt.Fprintln(os.Stderr, "a test wrote under the package's stand-in home, "+
			"which is the real ~/.brig outside tests; give it scratchHome:")
		for _, path := range reached {
			fmt.Fprintln(os.Stderr, "  "+path)
		}
		code = 1
	}
	_ = os.RemoveAll(home)
	os.Exit(code)
}

// scratchHome gives a test a home of its own, with the gateway directory
// inside it, and returns the home. BRIG_GATEWAY_SOCK is cleared too, because
// gatewaySocket prefers it to the directory for the shared gateway.
//
// Not t.TempDir(), which names the directory after the test: on macOS that
// alone can push a socket path past what one fits in. isolatedSocket then
// fails, the release returns without doing anything, and a test of it passes
// having proved nothing.
func scratchHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	dir := filepath.Join(home, ".brig")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("BRIG_GATEWAY_DIR", dir)
	t.Setenv("BRIG_GATEWAY_SOCK", "")
	return home
}

// plantGateways leaves a live gateway, with its records, under each name in
// whatever gateway directory the environment names now.
func plantGateways(t *testing.T, names ...string) map[string]int {
	t.Helper()
	pids := map[string]int{}
	for _, name := range names {
		sock := mustSocket(t, name)
		if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
			t.Fatal(err)
		}
		pid := fakeGateway(t, sock)
		writeGatewayRecord(sock, pid, gatewaySpec(0, Egress{Default: "deny"}))
		pids[sock] = pid
	}
	return pids
}

// The boots, stops and removals the tests drive must stay inside the test.
// A developer's gateways stand in the directory their environment names,
// under the names the tests boot, and every shape of release the tests reach
// is driven after scratchHome. Those gateways must still be up with their
// records in place.
//
// The same gateways planted inside the scratch home are the control: they
// must be gone, or the release never ran and the first half proves nothing.
func TestBootsUnderTestLeaveTheDevelopersGatewaysAlone(t *testing.T) {
	shapes := map[string]func(t *testing.T, decoy string){
		"HOME": func(t *testing.T, decoy string) {
			t.Setenv("HOME", decoy)
			t.Setenv("BRIG_GATEWAY_DIR", "")
			t.Setenv("BRIG_GATEWAY_SOCK", "")
		},
		"BRIG_GATEWAY_DIR": func(t *testing.T, decoy string) {
			t.Setenv("BRIG_GATEWAY_DIR", filepath.Join(decoy, ".brig"))
			t.Setenv("BRIG_GATEWAY_SOCK", filepath.Join(decoy, ".brig", "gateway.sock"))
		},
	}
	for shape, developer := range shapes {
		t.Run(shape, func(t *testing.T) {
			decoy, err := os.MkdirTemp("", "d")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(decoy) })
			developer(t, decoy)
			theirs := plantGateways(t, "brig-x", "vm")

			scratchHome(t)
			ours := plantGateways(t, "brig-x", "vm")

			up := &hull{bin: stubRuntimeBin(t, "", 0)}
			down := &hull{bin: stubRuntimeBin(t, "Error: boom", 1)}
			if err := up.Run(RunSpec{Name: "brig-x", Image: "img", Hypervisor: "vz"}); err != nil {
				t.Fatalf("vz boot: %v", err)
			}
			if err := down.Run(RunSpec{Name: "brig-x", Image: "img", Hypervisor: "vz"}); err == nil {
				t.Fatal("a runtime that exited non-zero must fail the boot")
			}
			if err := up.Run(RunSpec{Name: "vm", Image: "img", Mem: 2048, CPUs: 2}); err != nil {
				t.Fatalf("boot: %v", err)
			}
			if err := up.Stop("vm"); err != nil {
				t.Fatalf("stop: %v", err)
			}
			if err := up.Remove("brig-x"); err != nil {
				t.Fatalf("remove: %v", err)
			}

			for sock, pid := range ours {
				waitGone(t, pid, sock)
			}
			for sock, pid := range theirs {
				if !ownsGateway(pid, sock) {
					t.Errorf("a boot under test stopped the gateway at %s", sock)
				}
				for _, path := range []string{gatewayPIDPath(sock), gatewaySpecPath(sock)} {
					if _, err := os.Stat(path); err != nil {
						t.Errorf("a boot under test removed %s: %v", path, err)
					}
				}
			}
		})
	}
}
