// Command brig runs a coding agent in a sandbox.
//
// The sandbox is a microVM on macOS and a container on Linux, and brig is
// neither: it resolves your credentials on the host, forwards them in per
// exec, mounts a workspace as the agent's home, verifies the guest image, and
// delegates every mechanical operation to hull or nerdctl underneath. The
// product logic lives in internal/wrap and is shared by both, so the two
// operating systems cannot drift apart.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/brig-sh/brig/internal/agent"
	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/runtime"
	"github.com/brig-sh/brig/internal/wrap"
)

// version is stamped at build time by goreleaser.
var version = "dev"

// sandboxPrefix is how brig recognises its own sandboxes in a runtime that
// may be running other things.
const sandboxPrefix = "brig-"

const usage = `brig -- run a coding agent in a sandbox

usage:
  brig run    <agent> [flags] [agent args...]   start the sandbox and run the agent
  brig create <agent> [flags]                   start the sandbox without attaching
  brig exec   <agent> -- cmd [args...]          run one command inside the sandbox
  brig shell  <agent> [cmd...]                  open a shell inside the sandbox
  brig stop   <agent>                           stop the sandbox, keep it
  brig rm     <agent>                           stop and remove the sandbox
  brig ls                                       list sandboxes
  brig reset                                    stop and remove every brig sandbox
  brig env    <agent>                           show what would be forwarded, by name
  brig agents                                   list the agent templates
  brig template ls|export|import|rm             manage agent templates
  brig export <agent> [--json]                 print a template (YAML by default)
  brig version

flags (before the agent's own arguments; -- ends brig's parsing):
  -n, --name NAME        a session of its own: own workspace, own sandbox
  -t, --image IMAGE      guest image to boot
  -w, --workspace PATH   host directory to mount as the guest home
  -m, --memory MB        guest memory
      --cpus N           guest vCPUs
  -d, --detach           with run: start the sandbox and exit

Workspaces persist. The sandbox keeps running between commands, so a second
run is immediate; state lives in the workspace on the host either way.

Any Linux CLI in an OCI image already runs under brig. A template just saves
you spelling out the image and its credential variables every time: export the
closest one, edit it, import it back. Building an image for one is documented
at
  https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md

settings (BRIG_<AGENT>_<KEY> wins over BRIG_<KEY>; see the README for all):
  BRIG_WORKSPACE       host directory mounted as the guest home
  BRIG_IMAGE           guest image
  BRIG_PULL            missing (default) | always | never
  BRIG_FORWARD_ENV     variables to forward, space-separated
  BRIG_CREDENTIALS_CMD command printing the host credential JSON on stdout
  BRIG_GIT_CONFIG      1 to write the guest git-over-HTTPS files
  BRIG_VERIFY          warn (default) | require | off -- guest image signature
  BRIG_TEMPLATE_DIR    where custom templates live
  BRIG_RUNTIME         hull | nerdctl
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "brig: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	verb, rest := args[0], args[1:]

	// Custom templates are read before anything looks a name up, so one can
	// stand in for a built-in. A broken file is reported and skipped rather
	// than taking down the agent you were actually asking for.
	if err := agent.LoadCustom(agent.TemplateDir()); err != nil {
		fmt.Fprintln(os.Stderr, "brig: "+err.Error())
	}

	switch verb {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	case "version", "--version":
		fmt.Printf("brig %s\n", version)
		return nil
	case "agents":
		return listAgents()
	case "template":
		return templateCmd(rest)
	case "import":
		return importTemplate(rest)
	case "export":
		return exportTemplate(rest)
	case "ls":
		return listSandboxes()
	case "reset":
		return reset()
	case "run", "create", "exec", "shell", "stop", "rm", "env":
	default:
		return fmt.Errorf("unknown command %q (try `brig help`)", verb)
	}

	opts, template, tail, err := parse(rest)
	if err != nil {
		return err
	}
	t, ok := agent.Lookup(template)
	if !ok {
		if template == "" {
			return fmt.Errorf("%s needs an agent, for example `brig %s claude`. "+
				"`brig agents` lists them", verb, verb)
		}
		return fmt.Errorf("unknown agent %q. `brig agents` lists them", template)
	}

	rt, err := runtime.Detect()
	if err != nil {
		return err
	}
	cfg, err := wrap.Load(t, opts.load, rt)
	if err != nil {
		return err
	}
	// A ten-character slug is a tight budget, so two long names can slug the
	// same and share one sandbox. Say which directory a run actually uses
	// whenever the slug differs from the name, so that sharing stays visible.
	if cfg.RawName != "" && cfg.RawName != cfg.Slug {
		fmt.Fprintf(os.Stderr, "brig: session %q uses %s (sandbox %s)\n",
			cfg.RawName, cfg.Workspace, cfg.VMName)
	}

	// Stopping and removing need nothing but the instance name, and are
	// handled ahead of any credential resolution so they cannot raise the
	// keychain prompt that reading the host login may bring up.
	switch verb {
	case "stop":
		return cfg.Stop()
	case "rm":
		return cfg.Remove()
	}

	set, err := cfg.BuildEnv()
	if err != nil {
		return err
	}

	switch verb {
	case "env":
		cfg.Status(set)
		return nil
	case "create":
		if err := cfg.EnsureRunning(set); err != nil {
			return err
		}
		fmt.Println(cfg.VMName)
		return nil
	case "shell":
		if err := cfg.EnsureRunning(set); err != nil {
			return err
		}
		return cfg.Shell(set, tail)
	case "exec":
		if len(tail) == 0 {
			return errors.New("exec needs a command, for example `brig exec claude -- ls`")
		}
		if err := cfg.EnsureRunning(set); err != nil {
			return err
		}
		return cfg.Exec(set, tail, isTerminal())
	}
	return runAgent(cfg, set, t, tail, opts.detach)
}

func runAgent(cfg *wrap.Config, set creds.Set, t agent.Template, tail []string, detach bool) error {
	// A windowed agent owns its own console, so there is nothing to pass
	// through and nothing to exec into: starting it IS the command.
	if t.GUI && len(tail) > 0 {
		return fmt.Errorf("%s is a graphical agent, so it takes no arguments "+
			"(use `brig shell %s` or `brig stop %s`)", t.Name, t.Name, t.Name)
	}
	if err := cfg.EnsureRunning(set); err != nil {
		return err
	}
	switch {
	case t.GUI:
		fmt.Fprintf(os.Stderr, "brig: sandbox %s is running; the %s window should be visible.\n",
			cfg.VMName, t.GUITitle)
		return nil
	case detach:
		// The sandbox is up and will stay up; print the name so a script can
		// exec into it.
		fmt.Println(cfg.VMName)
		return nil
	case t.Shell:
		// The "agent" is the guest shell itself, so a bare run is a login
		// shell and trailing words are one command.
		return cfg.Shell(set, tail)
	}
	argv := append([]string{t.Binary}, agentArgs(cfg, t, tail)...)
	return cfg.Exec(set, argv, isTerminal())
}

// agentArgs adds the agent's own session-name flag, so the name you typed
// travels in unchanged as the display name while only the paths use the slug.
func agentArgs(cfg *wrap.Config, t agent.Template, tail []string) []string {
	if cfg.RawName != "" && t.Name == "claude-code" {
		return append([]string{"--name", cfg.RawName}, tail...)
	}
	return tail
}

// options are brig's own flags for one invocation.
type options struct {
	load      wrap.Options
	nameGiven bool
	detach    bool
}

// parse reads brig's own flags off the front in any order and leaves whatever
// remains for the agent. Scanning stops at the first argument that is not one
// of brig's, so `brig run claude -p hi` hands -p hi straight through, and `--`
// ends brig's parsing outright.
func parse(args []string) (o options, template string, tail []string, err error) {
	i := 0
	value := func(flag string) (string, bool) {
		if i+1 >= len(args) {
			err = fmt.Errorf("%s needs a value", flag)
			return "", false
		}
		v := args[i+1]
		i += 2
		return v, true
	}
	number := func(flag string, raw string) int {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n <= 0 {
			err = fmt.Errorf("%s needs a positive number, not %q", flag, raw)
		}
		return n
	}
	for i < len(args) && err == nil {
		a := args[i]
		flag, inline, hasInline := strings.Cut(a, "=")
		get := func() (string, bool) {
			if hasInline {
				i++
				return inline, true
			}
			return value(flag)
		}
		switch flag {
		case "--name", "-n":
			v, ok := get()
			if !ok {
				continue
			}
			o.load.Name, o.nameGiven = v, true
		case "--image", "-t":
			v, ok := get()
			if !ok {
				continue
			}
			o.load.Image = v
		case "--workspace", "-w":
			v, ok := get()
			if !ok {
				continue
			}
			o.load.Workspace = v
		case "--memory", "-m":
			v, ok := get()
			if !ok {
				continue
			}
			o.load.Mem = number(flag, v)
		case "--cpus":
			v, ok := get()
			if !ok {
				continue
			}
			o.load.CPUs = number(flag, v)
		case "--detach", "-d":
			o.detach = true
			i++
		case "--":
			i++
			return o, template, args[i:], err
		default:
			if template == "" && !strings.HasPrefix(a, "-") {
				template = a
				i++
				continue
			}
			return o, template, args[i:], err
		}
	}
	if err != nil {
		return options{}, "", nil, err
	}
	// An empty name is tracked apart from the value, because `--name ''`
	// otherwise reads exactly like passing no flag and would quietly run the
	// unnamed sandbox.
	if o.nameGiven && o.load.Name == "" {
		return options{}, "", nil, errors.New("--name needs a session name, for example `--name foo`")
	}
	return o, template, nil, nil
}

// listSandboxes shows what is running, and what is merely holding a name. A
// stopped sandbox still owns its name, which is exactly the thing to see
// before wondering why a name is taken.
func listSandboxes() error {
	rt, err := runtime.Detect()
	if err != nil {
		return err
	}
	list, err := rt.List()
	if err != nil {
		return err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	fmt.Printf("%-28s %-10s %s\n", "SANDBOX", "STATE", "WORKSPACE")
	shown := 0
	for _, inst := range list {
		if !strings.HasPrefix(inst.Name, sandboxPrefix) {
			continue
		}
		shown++
		fmt.Printf("%-28s %-10s %s\n", inst.Name, inst.State, workspaceOf(inst.Name, rt))
	}
	if shown == 0 {
		fmt.Println("(none -- `brig run claude` starts one)")
	}
	return nil
}

// workspaceOf recovers the workspace for a sandbox by asking the template it
// was named after. A sandbox brig did not name has none to report.
func workspaceOf(vmName string, rt runtime.Runtime) string {
	rest := strings.TrimPrefix(vmName, sandboxPrefix)
	// Longest template name first, so claude-code wins over a hypothetical
	// claude when both could prefix-match.
	names := agent.Names()
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for _, name := range names {
		if rest != name && !strings.HasPrefix(rest, name+"-") {
			continue
		}
		t, _ := agent.Lookup(name)
		cfg, err := wrap.Load(t, wrap.Options{Name: strings.TrimPrefix(rest, name+"-")}, rt)
		if err != nil {
			return ""
		}
		return cfg.Workspace
	}
	return ""
}

// reset stops and removes every sandbox brig started. Workspaces are left
// alone: they are on the host, they hold your work, and this is a command
// about sandboxes.
func reset() error {
	rt, err := runtime.Detect()
	if err != nil {
		return err
	}
	list, err := rt.List()
	if err != nil {
		return err
	}
	removed := 0
	for _, inst := range list {
		if !strings.HasPrefix(inst.Name, sandboxPrefix) {
			continue
		}
		_ = rt.Stop(inst.Name)
		if err := rt.Remove(inst.Name); err != nil {
			fmt.Fprintf(os.Stderr, "brig: could not remove %s: %v\n", inst.Name, err)
			continue
		}
		fmt.Println(inst.Name)
		removed++
	}
	fmt.Fprintf(os.Stderr, "brig: removed %d sandbox(es). Workspaces are untouched.\n", removed)
	return nil
}

func listAgents() error {
	for _, t := range agent.All() {
		suffix := ""
		if agent.IsCustom(t.Name) {
			suffix = "  (custom)"
		}
		fmt.Printf("%-15s %s%s\n", t.Name, t.Desc, suffix)
		fmt.Printf("%-15s image %s, home %s\n", "", t.Image, t.GuestHome)
		if len(t.Forward) > 0 {
			fmt.Printf("%-15s forwards: %s\n", "", strings.Join(t.Forward, " "))
		}
		if len(t.Deny) > 0 {
			fmt.Printf("%-15s never forwarded: %s\n", "", strings.Join(t.Deny, " "))
		}
	}
	fmt.Printf("\ncustom templates live in %s\n", agent.TemplateDir())
	fmt.Printf("to add one: brig export claude-code > mine.json, edit it, brig import mine.json\n")
	fmt.Printf("to build an image for one: %s\n", agent.BringYourOwnImageDoc)
	return nil
}

// templateCmd groups the template verbs, which is where someone coming from
// another sandbox tool will look for them.
func templateCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("template needs a subcommand: ls, export, import or rm")
	}
	switch args[0] {
	case "ls":
		return listAgents()
	case "export", "save":
		return exportTemplate(args[1:])
	case "import", "load":
		return importTemplate(args[1:])
	case "rm":
		return removeTemplate(args[1:])
	default:
		return fmt.Errorf("unknown template subcommand %q (ls, export, import, rm)", args[0])
	}
}

// importTemplate adds a custom template. Reading from - lets it come out of a
// pipe, which is what makes `brig export x | edit | brig import -` work.
func importTemplate(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("import needs a file, for example `brig import mine.json` "+
			"(or - to read stdin). See %s", agent.BringYourOwnImageDoc)
	}
	var blob []byte
	var err error
	if args[0] == "-" {
		blob, err = io.ReadAll(os.Stdin)
	} else {
		blob, err = os.ReadFile(args[0])
	}
	if err != nil {
		return err
	}
	dir := agent.TemplateDir()
	t, path, err := agent.Import(blob, dir)
	if err != nil {
		return fmt.Errorf("%w\n\nA template needs at least a name, an image, a guest home, "+
			"a binary, mem and cpus. `brig export claude-code` prints a working one to "+
			"start from, and %s explains how to build the image", err, agent.BringYourOwnImageDoc)
	}
	fmt.Printf("imported %s -> %s\n", t.Name, path)
	fmt.Printf("run it with: brig run %s\n", t.Name)
	if !strings.HasPrefix(t.Image, "ghcr.io/brig-sh/") {
		fmt.Printf("note: %s is not one of our images, so brig cannot verify its "+
			"signature. It will say so on every boot.\n", t.Image)
	}
	return nil
}

// exportTemplate prints a template. YAML by default, because the result is
// meant to be edited; --json for anything consuming it programmatically.
func exportTemplate(args []string) error {
	asJSON := false
	var name string
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		default:
			if name == "" {
				name = a
			}
		}
	}
	if name == "" {
		return errors.New("export needs an agent, for example `brig export claude-code`")
	}
	t, ok := agent.Lookup(name)
	if !ok {
		return fmt.Errorf("unknown agent %q. `brig agents` lists them", name)
	}
	render := agent.Export
	if asJSON {
		render = agent.ExportJSON
	}
	blob, err := render(t)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(blob)
	return err
}

// removeTemplate deletes a custom template. A built-in is compiled in and
// cannot be removed, only shadowed -- say so rather than reporting a missing
// file.
func removeTemplate(args []string) error {
	if len(args) == 0 {
		return errors.New("template rm needs a name")
	}
	name := args[0]
	if !agent.IsCustom(name) {
		if _, builtin := agent.Lookup(name); builtin {
			return fmt.Errorf("%s is a built-in template, so there is nothing to remove. "+
				"Import a template of the same name to shadow it", name)
		}
		return fmt.Errorf("no custom template named %q", name)
	}
	removed := false
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		path := filepath.Join(agent.TemplateDir(), name+ext)
		if err := os.Remove(path); err == nil {
			fmt.Printf("removed %s\n", path)
			removed = true
		}
	}
	if !removed {
		return fmt.Errorf("no template file for %q in %s", name, agent.TemplateDir())
	}
	return nil
}

// isTerminal reports whether stdin is a tty, which decides whether the guest
// exec allocates one. A headless `-p` run piped from a script must not get a
// pseudo-terminal.
func isTerminal() bool { return wrap.IsTerminal(os.Stdin) }
