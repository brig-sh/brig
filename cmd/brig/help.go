package main

// The usage text of each top-level verb, answered by both spellings that ask
// for it: `brig <verb> --help` (or -h) and `brig help <verb>`. One text per
// verb means the two answers cannot drift, and the global `usage` stays the
// command list and the flags the verbs share.
//
// The verb groups (agent, secret, policy, network, telemetry, completion) keep
// their own usage const beside their command and are named in verbUsages too,
// so `brig help agent` answers like `brig agent --help` rather than falling
// back to the global text.

const runUsage = `brig run -- start a sandbox and run an agent

usage:
  brig run <ref> [project] [args...]

A project is mounted read-write at /work/<name>, and the agent starts there.
Brig's own flags come before the ref; everything from the first agent argument
on is the agent's, and -- ends brig's parsing.

flags:
      --image IMAGE   guest image to boot
      --home PATH     host directory to mount as the guest home
      --no-project    mount no project this session, even one it ran with last
      --mem MB        guest memory
      --cpus N        guest vCPUs
  -d, --detach        start the sandbox and exit, without attaching
      --skills        copy your ~/.claude skills and plugins into the guest home
      --network MODE  shared, isolated or offline (or BRIG_NETWORK)
      --offline       shorthand for --network offline
      --publish PORT  open a guest port on the host; repeatable
      --json          run the agent as a child and print one JSON line after it

brig help lists every command, every flag and every setting.
`

const shUsage = `brig sh -- a login shell or one command inside the sandbox

usage:
  brig sh <ref> [command...]

With no command brig starts the sandbox if it is not running and opens a login
shell. With a command it runs that through the shell instead. The first bare
word after the ref is the guest command, so brig's own flags come before it.

flags:
      --image IMAGE   guest image to boot
      --home PATH     host directory to mount as the guest home
      --mem MB        guest memory
      --cpus N        guest vCPUs
      --skills        copy your ~/.claude skills and plugins into the guest home
      --network MODE  shared, isolated or offline (or BRIG_NETWORK)
      --offline       shorthand for --network offline
      --publish PORT  open a guest port on the host; repeatable
      --json          run the command as a child and print one JSON line after it

brig help lists every command, every flag and every setting.
`

const stopUsage = `brig stop -- stop the sandbox and keep it

usage:
  brig stop <ref>

Stopping a sandbox that is not running is not an error: that is already the
state stop asks for. The name, the workspace and the guest home survive.

brig help lists every command, every flag and every setting.
`

const rmUsage = `brig rm -- stop and remove the sandbox

usage:
  brig rm <ref> [--dry-run]
  brig rm --all [--dry-run] [-y]

One ref removes that sandbox. --all lists every brig sandbox, asks, then stops
and removes them. When brig created the guest home, rm deletes it too and prints
its path; a guest home you named and your project are host directories, and are
left alone.

flags:
      --dry-run  report what would be removed, and stop
  -y, --yes      with --all: the answer, given in advance

brig help lists every command, every flag and every setting.
`

const logsUsage = `brig logs -- stream a sandbox's log

usage:
  brig logs <ref> [--follow] [--tail N] [--raw]
  brig logs --gateway [<ref>]

Streams the sandbox's log. Given --gateway it reads the gateway's log instead:
the shared one, or with a ref the gateway serving that sandbox alone.

flags:
      --follow   keep the stream open and print lines as they appear
      --tail N   print the last N lines (all of them by default)
      --raw      print the runtime's bytes without filtering terminal control
                 sequences
      --gateway  read the gateway's log instead of the sandbox's

brig help lists every command, every flag and every setting.
`

const infoUsage = `brig info -- print the execution envelope and the environment

usage:
  brig info <ref>

Prints what a run would boot and which credential variables it would forward,
by name, without booting anything. It fails if a declared secret is missing.
The run-line flags below change the envelope the same way they would change a
run, so brig info --image IMAGE claude previews that image.

flags:
      --image IMAGE   guest image to boot
      --home PATH     host directory to mount as the guest home
      --network MODE  shared, isolated or offline (or BRIG_NETWORK)
      --offline       shorthand for --network offline
      --publish PORT  open a guest port on the host; repeatable
      --json          print the envelope as JSON

brig help lists every command, every flag and every setting.
`

const lsUsage = `brig ls -- list sandboxes

usage:
  brig ls [-q] [--json]

Lists every sandbox brig has, with its ref, state and guest home.

flags:
  -q, --quiet  print the refs only, one per line, for a script
      --json   print the listing as JSON

brig help lists every command, every flag and every setting.
`

const doctorUsage = `brig doctor -- check the host, the runtime and an agent's image

usage:
  brig doctor [<agent>] [--json]

Reports, in the order a boot hits them, the prerequisites a first run can fail
on. Given an agent, it also checks that agent's image.

flags:
      --json  print the report as JSON

brig help lists every command, every flag and every setting.
`

const versionUsage = `brig version -- print the brig build

usage:
  brig version [--json]

One line naming the tag, the commit and the Go platform, or the same build as
JSON.

flags:
      --json  print the build as JSON

brig help lists every command, every flag and every setting.
`

// verbUsages is the usage each top-level verb answers with, keyed by the word
// the reader typed. `brig <verb> --help` (or -h) and `brig help <verb>` read
// the same entry, so the two spellings cannot drift apart. The groups keep
// their const beside their command and are named here too.
var verbUsages = map[string]string{
	"run":        runUsage,
	"sh":         shUsage,
	"stop":       stopUsage,
	"rm":         rmUsage,
	"logs":       logsUsage,
	"info":       infoUsage,
	"ls":         lsUsage,
	"doctor":     doctorUsage,
	"version":    versionUsage,
	"agent":      agentUsage,
	"secret":     secretUsage,
	"policy":     policyUsage,
	"network":    networkUsage,
	"telemetry":  telemetryUsage,
	"completion": completionUsage,
}
