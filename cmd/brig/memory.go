package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/brig-sh/brig/internal/injector"
	"github.com/brig-sh/brig/internal/memory"
	"github.com/brig-sh/brig/internal/wrap"
)

// memoryUsage is what `brig memory --help` prints.
//
// The verb is `memory` and the run flag is --agent-memory. --memory is the
// older spelling of --mem, the guest's RAM, so the two words stay apart: a
// bare `memory` in verb position is this group, and `brig run --memory 2048`
// is still a size.
const memoryUsage = `brig memory -- list and remove the agent memory stores

usage:
  brig memory ls [--json]                 list the stores on this host
  brig memory rm <store>                  remove a store and its memories
  brig memory rm --project <dir>          remove the store of a project folder

A store holds what an agent remembers between sessions. It lives under brig's
state directory, outlives the sandbox that used it, and is attached to a new
one with brig run --agent-memory[=NAME].

<store> is the name of a named store, or the project-<hex> key that ls prints
for a project's store. rm refuses a store that a sandbox was started with until
you remove that sandbox: brig rm <ref>.

flags:
      --json     with ls: print the stores as JSON
      --project  with rm: the project folder whose store to remove

brig help lists every command, every flag and every setting.
`

// memoryCmd is the memory group: ls and rm.
func memoryCmd(out io.Writer, args []string) error {
	if len(args) == 0 {
		return errors.New("memory needs a subcommand: ls or rm")
	}
	var err error
	switch args[0] {
	case "--help", "-h", "help":
		_, err = io.WriteString(out, memoryUsage)
		return err
	case "ls":
		err = listMemory(out, args[1:])
	case "rm":
		err = removeMemory(out, args[1:])
	default:
		return fmt.Errorf("unknown memory subcommand %q (ls, rm)", args[0])
	}
	// The flag package reports --help as an error. Help is printed with exit
	// code zero, as the other groups do.
	if errors.Is(err, flag.ErrHelp) {
		_, err = io.WriteString(out, memoryUsage)
	}
	return err
}

// memoryJSON is one store as data, the MemoryList row. inUse is the sandboxes
// a session still names that were started with the store, [] when there are
// none, so a parser never meets null.
type memoryJSON struct {
	Store    string   `json:"store"`
	Project  string   `json:"project"`
	Memories int      `json:"memories"`
	InUse    []string `json:"inUse"`
}

func listMemory(out io.Writer, args []string) error {
	jsonOut := globalJSON
	for _, a := range args {
		switch {
		case a == "--json":
			jsonOut = true
		case isHelp(a):
			return flag.ErrHelp
		default:
			return usagef("unexpected argument %q; `brig memory ls` takes no "+
				"arguments other than --json", a)
		}
	}
	root, err := injector.MemoryDir()
	if err != nil {
		return err
	}
	stores, err := memory.List(root)
	if err != nil {
		return err
	}
	rows := make([]memoryJSON, 0, len(stores))
	for _, s := range stores {
		rows = append(rows, memoryJSON{
			Store:    s.Key,
			Project:  s.Project,
			Memories: s.Memories,
			InUse:    append([]string{}, wrap.MemoryUsers(s.Key)...),
		})
	}
	if jsonOut {
		// An empty list is data: [] and exit 0.
		return writeJSONDocument(out, "MemoryList", rows)
	}
	// Nothing to show, nothing printed: a header over no rows reads as a
	// listing that failed to load.
	if len(rows) == 0 {
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STORE\tPROJECT\tMEMORIES\tIN USE")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", r.Store, dash(r.Project), r.Memories, dash(strings.Join(r.InUse, ",")))
	}
	return w.Flush()
}

// servedMemoryUsers refuses to remove store key while the running broker
// serves memory to a sandbox recorded on it, or to a sandbox memory.json has
// no record of. memory.json is rewritten without a lock and reads as empty
// when it does not parse, so the broker's registrations are checked too: a
// lost record must not read as a store nothing uses.
func servedMemoryUsers(key string) error {
	dir, err := injector.Dir()
	if err != nil {
		return err
	}
	b, ok := injector.Running(dir)
	if !ok {
		return nil
	}
	recorded := wrap.MemoryOfSandboxes()
	for _, id := range b.Status.Memory {
		switch k, ok := recorded[id]; {
		case !ok:
			return fmt.Errorf("the broker serves memory to %s, and brig has no record of its store; "+
				"remove that sandbox, or stop it with `brig stop`, first", id)
		case k == key:
			return fmt.Errorf("store %s is in use by %s; remove that sandbox first", key, id)
		}
	}
	return nil
}

// dash is the table's placeholder for an empty cell.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func removeMemory(out io.Writer, args []string) error {
	fs := flag.NewFlagSet("memory rm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	project := fs.String("project", "", "")
	// Flags may follow the store, as they do on the other groups' lines.
	var words []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return err
			}
			msg := err.Error()
			if name, ok := strings.CutPrefix(msg, "flag provided but not defined: "); ok {
				msg = "unknown flag " + spell(name)
			} else {
				msg = rewriteFlagError(err).Error()
			}
			return usagef("%s (`brig memory rm` takes <store> or --project <dir>)", msg)
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	var key string
	switch {
	case *project != "" && len(words) > 0:
		return usagef("name a store or --project, not both")
	case *project != "":
		// Resolved the way a run resolves the project it mounts: made
		// absolute and cleaned by ProjectKey, links left alone.
		abs, err := filepath.Abs(*project)
		if err != nil {
			return err
		}
		key = memory.ProjectKey(abs)
	case len(words) == 1:
		key = words[0]
	case len(words) == 0:
		return usagef("`brig memory rm` needs a store: `brig memory rm <store>` " +
			"or `brig memory rm --project <dir>`")
	default:
		return usagef("unexpected argument %q; `brig memory rm` takes one store", words[1])
	}
	// The in-use check only reads brig's indexes. The name reaches the disk
	// through memory.Remove, which refuses anything but a store key before it
	// builds a path.
	if users := wrap.MemoryUsers(key); len(users) > 0 {
		return fmt.Errorf("store %s is in use by %s; remove that sandbox first",
			key, strings.Join(users, ", "))
	}
	if err := servedMemoryUsers(key); err != nil {
		return err
	}
	root, err := injector.MemoryDir()
	if err != nil {
		return err
	}
	switch err := memory.Remove(root, key); {
	case errors.Is(err, memory.ErrNotFound):
		return notFoundf("no memory store named %q. `brig memory ls` lists them", key)
	case errors.Is(err, memory.ErrInvalidName):
		return usagef("%s", err)
	case err != nil:
		return err
	}
	fmt.Fprintf(out, "removed store %s\n", key)
	return nil
}
