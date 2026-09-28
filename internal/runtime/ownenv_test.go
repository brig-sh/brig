package runtime

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// recordingRuntime writes a stand-in runtime binary that appends its own HOME
// and its argv to a log, one call per pair of lines, and returns both paths.
func recordingRuntime(t *testing.T) (bin, log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in is not portable to windows")
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, "runtime")
	log = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"{ printf 'home=%s\\n' \"$HOME\"; printf 'argv:'; printf ' %s' \"$@\"; printf '\\n'; } >> '" + log + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// adapterCalls returns one boot and one exec through an adapter, each carrying
// env to the guest.
func adapterCalls(rt func(bin string) Runtime, env []Var) map[string]func(bin string) error {
	return map[string]func(bin string) error{
		"run": func(bin string) error {
			return rt(bin).Run(RunSpec{Name: "brig-x", Image: "img", Mem: 1, CPUs: 1, Env: env})
		},
		"exec": func(bin string) error {
			_, err := rt(bin).Output(ExecSpec{Name: "brig-x", Cmd: []string{"true"}, Env: env})
			return err
		},
	}
}

// checkGuestHome runs a boot and an exec that carry the guest HOME next to a
// credential. The runtime must keep the host's HOME, the guest HOME must be on
// its command line, and the credential must not.
func checkGuestHome(t *testing.T, flag string, rt func(bin string) Runtime) {
	host := scratchHome(t)
	t.Setenv("BRIG_ENV_ARGV", "")
	env := []Var{{Name: "HOME", Value: "/root"}, {Name: "GH_TOKEN", Value: "ghp_secret"}}
	for name, call := range adapterCalls(rt, env) {
		t.Run(name, func(t *testing.T) {
			bin, log := recordingRuntime(t)
			if err := call(bin); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			got := string(b)
			if !strings.Contains(got, "home="+host+"\n") || strings.Contains(got, "home=/root\n") {
				t.Errorf("the runtime ran with the guest's HOME, want the host's %s:\n%s", host, got)
			}
			if !strings.Contains(got, " "+flag+" HOME=/root ") {
				t.Errorf("the guest HOME is not on the command line as %s HOME=/root:\n%s", flag, got)
			}
			if strings.Contains(got, "ghp_secret") {
				t.Errorf("a credential reached the command line:\n%s", got)
			}
		})
	}
}

// hull resolves its store under the HOME it runs with. Given the guest's, it
// tries to create /root on the sealed system volume and every boot and exec
// fails (#337).
func TestHullRunsWithTheHostHome(t *testing.T) {
	checkGuestHome(t, "--env", func(bin string) Runtime { return &hull{bin: bin} })
}

// A rootless nerdctl reads its registry config under the HOME it runs with,
// and a normal user cannot open /root/work/.docker/config.json.
func TestNerdctlRunsWithTheHostHome(t *testing.T) {
	checkGuestHome(t, "-e", func(bin string) Runtime { return &nerdctl{bin: bin} })
}

// A stored secret bound to HOME is refused before the runtime is started, on
// both adapters and on both paths. See splitEnv.
func TestAStoredSecretTheRuntimeReadsStartsNothing(t *testing.T) {
	// A failed hull boot withdraws the sandbox's publications, which reads the
	// gateway state under HOME. The directory and the socket are emptied after
	// scratchHome so that it is HOME the lookup falls back to.
	scratchHome(t)
	t.Setenv("BRIG_GATEWAY_DIR", "")
	t.Setenv("BRIG_GATEWAY_SOCK", "")
	env := []Var{{Name: "HOME", Value: "sk-fromkeychain", Secret: true}}
	adapters := map[string]func(bin string) Runtime{
		"hull":    func(bin string) Runtime { return &hull{bin: bin} },
		"nerdctl": func(bin string) Runtime { return &nerdctl{bin: bin} },
	}
	for kind, rt := range adapters {
		for name, call := range adapterCalls(rt, env) {
			t.Run(kind+" "+name, func(t *testing.T) {
				bin, log := recordingRuntime(t)
				err := call(bin)
				if err == nil || !strings.Contains(err.Error(), "HOME") {
					t.Errorf("want a refusal naming HOME, got %v", err)
				}
				if _, statErr := os.Stat(log); !errors.Is(statErr, fs.ErrNotExist) {
					t.Errorf("the runtime was started anyway")
				}
			})
		}
	}
}
