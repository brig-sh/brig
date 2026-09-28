package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// TestExitCode pins the mapping from an error to the process exit status. Every
// code in docs/cli.md's table has a case here, so a documented code that nothing
// returns is a test failure rather than a surprise for a script.
func TestExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil is success", nil, 0},
		{"a bare error is the general failure", errors.New("boom"), 1},
		{"a usage error", usagef("bad flag"), 2},
		{"a missing profile", notFoundf("unknown profile %q", "nope"), 3},
		{"no runtime on PATH", runtime.ErrNoRuntime, 4},
		{"a broken runtime", runtime.ErrBadRuntime, 4},
		{"a verification refusal", &wrap.VerifyRefusedError{Err: errors.New("refused")}, 5},
		{"an unresolved credential", &creds.MissingSecretsError{
			Sandbox: "brig-claude-code",
			Profile: "claude-code",
			Missing: []creds.Missing{{Name: "TOKEN"}},
		}, 6},
		{"a run path that cannot enforce a property", capabilityRefusal(runtime.CannotEnforce), 7},
		{"a run path whose answer is unknown", capabilityRefusal(runtime.Unknown), 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exitCode(c.err); got != c.want {
				t.Fatalf("exitCode = %d, want %d", got, c.want)
			}
		})
	}
}

// TestExitCodeThroughWrapping checks that the mapping reads the cause, not the
// outermost message: an error handed up the stack is wrapped more than once, and
// a code that only matched a bare sentinel would fall back to 1 the moment a
// caller added context.
func TestExitCodeThroughWrapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"wrapped usage", fmt.Errorf("context: %w", usagef("bad flag")), 2},
		{"wrapped not-found", fmt.Errorf("context: %w", notFoundf("unknown profile %q", "x")), 3},
		{"wrapped no-runtime", fmt.Errorf("context: %w", runtime.ErrNoRuntime), 4},
		{"wrapped bad-runtime", fmt.Errorf("context: %w", runtime.ErrBadRuntime), 4},
		{"wrapped verify", fmt.Errorf("context: %w", &wrap.VerifyRefusedError{Err: errors.New("no")}), 5},
		{"wrapped credentials", fmt.Errorf("context: %w", &creds.MissingSecretsError{Missing: []creds.Missing{{Name: "T"}}}), 6},
		// The boot path hands a refusal up as "could not start the sandbox: %w".
		{"wrapped capability", fmt.Errorf("could not start the sandbox: %w", capabilityRefusal(runtime.CannotEnforce)), 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exitCode(c.err); got != c.want {
				t.Fatalf("exitCode = %d, want %d", got, c.want)
			}
		})
	}
}

func capabilityRefusal(state runtime.Capability) error {
	return &runtime.CapabilityError{Property: runtime.EgressPolicy,
		Path: runtime.RunPath{Runtime: "hull", Backend: "vz"}, State: state,
		Why: "vmnet", Remedy: "Run it on hvi"}
}

// stubRuntime points DetectFor at a script that fails every command it is
// given, under the name the runtime goes by. The refusals below come from the
// adapters themselves, so the exit status is read off the error a real run
// gets, not off one the test built.
func stubRuntime(t *testing.T, kind, name string) runtime.Runtime {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	bin := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_RUNTIME", kind)
	t.Setenv("BRIG_RUNTIME_BIN", bin)
	t.Setenv("BRIG_CONTAINERD_RUNTIME", "")
	rt, err := runtime.DetectFor(runtime.Preference{})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// A policy on a run path that cannot enforce it exits 7 on every such path,
// and the same adapters' other refusals and failures do not. 7 tells a script
// "this runtime cannot keep a promise the run asked for", and a 7 on any other
// failure sends it looking for a policy problem that is not there.
func TestAPolicyNothingEnforcesExits7(t *testing.T) {
	policy := runtime.Egress{Default: "deny", Allow: []runtime.Rule{{Host: "example.com"}}}
	for _, c := range []struct {
		kind, name, hv string
	}{
		{"hull", "hull", "vz"},
		{"hull", "hull", "qemu"},
		{"hull", "hull", "krun"}, // no record, so unknown
		{"nerdctl", "nerdctl", ""},
		{"nerdctl", "docker", ""},
	} {
		t.Run(c.name+" "+c.hv, func(t *testing.T) {
			rt := stubRuntime(t, c.kind, c.name)
			err := rt.(runtime.RunChecker).CanRun(runtime.RunSpec{Name: "brig-exit",
				Hypervisor: c.hv, Net: "isolated", Egress: policy})
			if got := exitCode(err); got != 7 {
				t.Fatalf("exitCode = %d, want 7, for %v", got, err)
			}
		})
	}
	// brig network publish asks CanRun with the port added. vz refuses the
	// port too, and the policy is the refusal it reports.
	t.Run("hull vz with a port", func(t *testing.T) {
		rt := stubRuntime(t, "hull", "hull")
		pub, err := runtime.ParsePublication("8080")
		if err != nil {
			t.Fatal(err)
		}
		err = rt.(runtime.RunChecker).CanRun(runtime.RunSpec{Name: "brig-exit",
			Hypervisor: "vz", Net: "isolated", Egress: policy, Publish: []runtime.Publication{pub}})
		if got := exitCode(err); got != 7 {
			t.Fatalf("exitCode = %d, want 7, for %v", got, err)
		}
	})
}

// On hvi the table answers enforced and the gateway probe decides at boot. A
// probe that fails answers unknown, and that refusal comes up through Run and
// the boot path's wrap, not through CanRun. It exits 7 all the same.
func TestAFailedGatewayProbeExits7(t *testing.T) {
	rt := stubRuntime(t, "hull", "hull")
	err := rt.Run(runtime.RunSpec{Name: "brig-exit", Image: "img", Hypervisor: "hvi",
		Net: "isolated", Egress: runtime.Egress{Default: "deny"}})
	err = fmt.Errorf("could not start the sandbox: %w", err)
	if got := exitCode(err); got != 7 {
		t.Fatalf("exitCode = %d, want 7, for %v", got, err)
	}
}

func TestOtherRefusalsAndBootFailuresDoNotExit7(t *testing.T) {
	t.Run("a port on vz", func(t *testing.T) {
		rt := stubRuntime(t, "hull", "hull")
		pub, err := runtime.ParsePublication("8080")
		if err != nil {
			t.Fatal(err)
		}
		err = rt.(runtime.RunChecker).CanRun(runtime.RunSpec{Name: "brig-exit",
			Hypervisor: "vz", Net: "shared", Publish: []runtime.Publication{pub}})
		if err == nil {
			t.Fatal("vz took a published port")
		}
		if got := exitCode(err); got != 1 {
			t.Errorf("exitCode = %d, want 1, for %v", got, err)
		}
	})
	t.Run("a window on nerdctl", func(t *testing.T) {
		rt := stubRuntime(t, "nerdctl", "nerdctl")
		err := rt.(runtime.RunChecker).CanRun(runtime.RunSpec{Name: "brig-exit", GUI: true})
		if err == nil {
			t.Fatal("nerdctl took a graphical profile")
		}
		if got := exitCode(err); got != 1 {
			t.Errorf("exitCode = %d, want 1, for %v", got, err)
		}
	})
	// A boot with no policy that the runtime fails is the runtime failing,
	// not a property it cannot enforce.
	t.Run("a boot the runtime fails", func(t *testing.T) {
		rt := stubRuntime(t, "hull", "hull")
		err := rt.Run(runtime.RunSpec{Name: "brig-exit", Image: "img", Hypervisor: "vz", Net: "shared"})
		if err == nil {
			t.Fatal("a runtime that exits 1 booted")
		}
		err = fmt.Errorf("could not start the sandbox: %w", err)
		if got := exitCode(err); got != 1 {
			t.Errorf("exitCode = %d, want 1, for %v", got, err)
		}
	})
	// The class is the type. A message that reads like the refusal is not one.
	t.Run("the refusal's words without its type", func(t *testing.T) {
		err := errors.New(capabilityRefusal(runtime.CannotEnforce).Error())
		if got := exitCode(err); got != 1 {
			t.Errorf("exitCode = %d, want 1", got)
		}
	})
}

// A refusal brig reaches before the policy keeps its own code. A backend with
// no window refuses a graphical profile over the window first, so the run
// exits 1 under a policy too. That refusal names the first change to make:
// on vz the same run then gets the policy refusal, and exit 7.
func TestAWindowRefusalAnswersBeforeThePolicy(t *testing.T) {
	policy := runtime.Egress{Default: "deny", Allow: []runtime.Rule{{Host: "example.com"}}}
	for _, c := range []struct {
		kind, name, hv string
	}{
		{"hull", "hull", "qemu"},
		{"nerdctl", "nerdctl", ""},
	} {
		t.Run(c.name+" "+c.hv, func(t *testing.T) {
			rt := stubRuntime(t, c.kind, c.name)
			err := rt.(runtime.RunChecker).CanRun(runtime.RunSpec{Name: "brig-exit",
				Hypervisor: c.hv, Net: "shared", GUI: true, Egress: policy})
			if err == nil || !strings.Contains(err.Error(), "graphical window") {
				t.Fatalf("not the window refusal: %v", err)
			}
			if got := exitCode(err); got != 1 {
				t.Errorf("exitCode = %d, want 1, for %v", got, err)
			}
		})
	}
}

// bindPolicy binds a deny-by-default egress policy to profile, the way
// `brig policy attach` leaves it on disk.
func bindPolicy(t *testing.T, profile string) {
	t.Helper()
	dir := t.TempDir()
	doc := "apiVersion: brig.sh/v1alpha1\nname: no-net\negress:\n  default: deny\n"
	if err := os.WriteFile(filepath.Join(dir, "no-net.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	bound := "profiles:\n  " + profile + ": [no-net]\n"
	if err := os.WriteFile(filepath.Join(dir, "attachments.yaml"), []byte(bound), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_POLICY_DIR", dir)
}

// brig network publish asks the backend through CanPublish, a path of its own
// that never reaches Run. A wrap on that path that flattens the error to its
// words turns the 7 back into a 1. The refusal also has to land before the
// port is recorded, or the next boot opens a port on a sandbox whose policy
// nothing filters.
func TestNetworkPublishUnderAPolicyVzCannotEnforceExits7(t *testing.T) {
	rt := stubRuntime(t, "hull", "hull")
	jsonRunHost(t, rt)
	bindPolicy(t, "faker")
	t.Setenv("BRIG_HYPERVISOR", "vz")
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())

	_, err := captureStdout(t, func() error {
		return run([]string{"network", "publish", "faker", "3000"})
	})
	if got := exitCode(err); got != 7 {
		t.Fatalf("exitCode = %d, want 7, for %v", got, err)
	}
	if got, _ := runtime.Publications("brig-faker"); len(got) != 0 {
		t.Errorf("a refused publish recorded %v", got)
	}
}

// absentHull is the hull adapter driven by a script that knows no instance,
// so a run gets past Load to the backend check. It logs every command it is
// given, one line each, to the returned file.
func absentHull(t *testing.T) (runtime.Runtime, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	log := filepath.Join(t.TempDir(), "argv.log")
	bin := filepath.Join(t.TempDir(), "hull")
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\necho \"instance not found: $2\" >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIG_RUNTIME", "hull")
	t.Setenv("BRIG_RUNTIME_BIN", bin)
	rt, err := runtime.DetectFor(runtime.Preference{})
	if err != nil {
		t.Fatal(err)
	}
	return rt, log
}

// isolatedFaker gives jsonRunHost's faker the network: isolated that most
// shipped profiles carry, and leaves no setting to choose another posture.
func isolatedFaker(t *testing.T) {
	t.Helper()
	dir := os.Getenv("BRIG_PROFILE_DIR")
	body := "name: faker\nimage: img\nguestHome: /home/faker\nbinary: agent\nmem: 1024\ncpus: 1\nnetwork: isolated\n"
	if err := os.WriteFile(filepath.Join(dir, "faker.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := profile.Load(profile.Dir()); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"BRIG_NETWORK", "BRIG_FAKER_NETWORK"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

// vz and qemu refuse an isolated network as well as a policy, and the profile
// is what chose isolated here. The policy refusal is the one a script has to
// act on, so it answers first and exits 7, under --json too. The profile
// refusal used to answer first and exit 1.
func TestAPolicyOnAProfileThatSetsIsolatedExits7(t *testing.T) {
	for _, hv := range []string{"vz", "qemu"} {
		t.Run(hv, func(t *testing.T) {
			rt, log := absentHull(t)
			jsonRunHost(t, rt)
			isolatedFaker(t)
			bindPolicy(t, "faker")
			t.Setenv("BRIG_HYPERVISOR", hv)
			t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())

			_, err := captureStdout(t, func() error { return run([]string{"run", "faker", "-d"}) })
			var refusal *runtime.CapabilityError
			if !errors.As(err, &refusal) {
				t.Errorf("the refusal is not the policy's: %v", err)
			}
			if got := exitCode(err); got != 7 {
				t.Errorf("exitCode = %d, want 7, for %v", got, err)
			}

			var jsonErr error
			out, _ := captureStdout(t, func() error {
				jsonErr = run([]string{"run", "--json", "faker", "-d"})
				return nil
			})
			if got := exitCode(jsonErr); got != 7 {
				t.Errorf("run --json: exitCode = %d, want 7, for %v", got, jsonErr)
			}
			data, _ := lastJSONLine(t, out)["data"].(map[string]any)
			if data["stage"] != "brig" || data["exit"] != float64(7) {
				t.Errorf("run --json: stage %v exit %v, want brig and 7", data["stage"], data["exit"])
			}

			argv, err := os.ReadFile(log)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(argv), "\n") {
				if strings.HasPrefix(line, "run ") {
					t.Errorf("a refused run reached the runtime: %s", line)
				}
			}
		})
	}
}

// With no policy bound, the same profile on vz or qemu gets the isolated
// network refusal. That one names the profile and exits 1: no policy is
// involved, and 7 would send a script looking for one.
func TestTheProfileIsolationRefusalWithNoPolicyExits1(t *testing.T) {
	for _, hv := range []string{"vz", "qemu"} {
		t.Run(hv, func(t *testing.T) {
			rt, _ := absentHull(t)
			jsonRunHost(t, rt)
			isolatedFaker(t)
			t.Setenv("BRIG_POLICY_DIR", t.TempDir())
			t.Setenv("BRIG_HYPERVISOR", hv)
			t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())

			_, err := captureStdout(t, func() error { return run([]string{"run", "faker", "-d"}) })
			if err == nil || !strings.Contains(err.Error(), "the faker profile sets network: isolated") {
				t.Fatalf("not the profile's isolated network refusal: %v", err)
			}
			if got := exitCode(err); got != 1 {
				t.Errorf("exitCode = %d, want 1, for %v", got, err)
			}
		})
	}
}
