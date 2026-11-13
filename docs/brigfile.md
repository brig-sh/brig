# The Brigfile

A Brigfile is a file at the top of a repository that says how to run the
repository under Brig. It names profiles and policies by alias, and declares
one or more targets. A target pairs one profile with its policies, a size
and default arguments.

You can write a Brigfile, check it, see what each target resolves to, plan
a run of a target, run it once you approve it, and stop or remove a session
of a target.

## Start one

```bash
brig init                  # writes a Brigfile at the top of the worktree
brig target ls             # lists its targets
brig config --target dev   # shows what dev resolves to
brig plan --target dev     # shows what a run of dev would get
brig run --target dev      # asks you to approve that run, then runs it
```

`brig init` writes a starter with one target, `dev`, that runs the
`claude-code` profile Brig ships. It does not replace a file that is
already there.

## The format

```yaml
apiVersion: brig.sh/v1alpha1
name: demo
defaultTarget: dev
profiles:
  claude:
    from: builtin:claude-code
  tool:
    file: .brig/tool.yaml
policies:
  anthropic:
    file: .brig/anthropic.yaml
targets:
  dev:
    profile: claude
    policies: [anthropic]
    overrides:
      cpus: 2
    args: ["--continue"]
  ci:
    profile: tool
```

| Field | Required | Meaning |
| --- | --- | --- |
| `apiVersion` | yes | `brig.sh/v1alpha1` |
| `name` | no | A label for display |
| `defaultTarget` | no | The target to use when none is named. It must name a target |
| `profiles` | no | Profile aliases. Each one is declared in one of three forms, below |
| `policies` | no | Policy aliases. Each one is `file:` or `inline:` |
| `targets` | yes | At least one target |

A target has these fields:

| Field | Required | Meaning |
| --- | --- | --- |
| `profile` | yes | A profile alias |
| `policies` | no | Policy aliases, each at most once |
| `overrides` | no | `cpus`, `mem` (in MB) or `image`, in place of the profile's value |
| `args` | no | The arguments the agent gets. Arguments after `--` on the run line replace them |

An alias or a target name uses lowercase letters, digits, `.`, `-` and `_`,
and starts with a letter or a digit.

Brig reads the file strictly. It refuses an unknown key, a key given twice,
and a value that YAML reads as a number or a boolean where a string goes.
Quote a value such as `"0600"` or `"yes"`. The error names the file and the
field.

The schema for editors is
[`api/project/v1alpha1/brigfile.schema.json`](../api/project/v1alpha1/brigfile.schema.json).

## Profile and policy forms

| Form | What it reads |
| --- | --- |
| `from: builtin:<name>` | A profile Brig ships, by its full name, such as `builtin:claude-code`. Profiles only |
| `file: <path>` | A profile or policy document in the repository |
| `inline:` | A complete profile or policy document, written in the Brigfile |

`builtin:<name>` is the same profile on every machine. If your own profile
directory has a file that overrides that built-in, the Brigfile does not
read it. `brig config` prints a note when that happens.

A `file:` path is relative to the directory that holds the Brigfile. Write
it with `/`, and with no `..`, no leading `/`, no leading `~` and no control
character. Brig reads only regular files below that directory, and refuses a
link that leads out of it.

A profile document is the same format as your own profiles. See
[Writing an agent profile](profiles.md). A policy document is described in
[Policies](policies.md).

## Overrides and policy order

A value in a target's `overrides` replaces the profile's value. A field the
target does not override comes from the profile. `brig config --target`
shows the origin of each value.

An `image` override on a `builtin:` profile keeps what that profile binds.
The image the repository names then receives the credentials Brig binds for
the built-in. `brig plan --target` says so in a `NOTE` row and names each
one.

A target's policies come in this order: the target's own `policies`, then
the ones a repository profile lists under `policy:`. Each alias appears
once.

A target on a `shell` or `gui` profile cannot list policies. Brig does not
enforce a policy on those profiles, so it refuses the target.

## Plan a target

```bash
brig plan --target dev
brig plan --target dev --json
brig plan --target ci -f ci/Brigfile.ci
```

`brig plan --target <name>` prints the permissions that a run of the target
would get. It boots nothing, opens no secret and writes nothing. The project
is the directory that holds the Brigfile.

A repository can change its Brigfile, and so can an agent that runs in the
repository. Read the plan before you run a target. The plan shows:

- the target, its Brigfile, and the profile with its source
- where the image, the memory, the vCPUs and the network come from
- whether the image is pinned by digest
- each policy, and where it comes from
- for each route, where it sends requests and which secrets in your store it
  uses

`brig plan --target` takes the flags `--session`, `--image`, `--mem`,
`--cpus`, `--network`, `--offline`, `--publish`, `--skills` and `--json`. It
takes no profile and no project, and it refuses `--home` and `--no-project`.
[`brig plan`](cli.md#brig-plan) describes each row and field.

### Where each value comes from

The image, the memory and the vCPUs come from the first of these that sets
them:

1. a flag on the command line
2. a `BRIG_*` setting
3. the target's `overrides`
4. the profile

The network follows the same order, without step 3. The plan names the
source of each value.

### Which settings apply

For a target on a `builtin:` profile, the settings of that profile apply,
such as `BRIG_CLAUDE_CODE_IMAGE`. The global settings, such as `BRIG_IMAGE`,
also apply.

For a target on a `file:` or `inline:` profile, only the global settings
apply. A repository profile named `claude-code` does not read
`BRIG_CLAUDE_CODE_IMAGE` or any other `BRIG_CLAUDE_CODE_*` setting.

`BRIG_NAME` and `BRIG_WORKSPACE` do not apply to a target. Each session of a
target has its own sandbox and its own guest home, named after the project,
the target and the session. The sandbox name of a target starts with
`brig--`. See [Sessions of a target](#sessions-of-a-target).

### Which policies apply

A target gets these policies:

1. the target's own `policies`
2. the policies that a repository profile lists under `policy:`
3. for a `builtin:` profile only, the policies in that profile's own
   `policy:` list and the policies you attached to that profile with
   `brig policy attach`

Brig reads the Brigfile's policies from the repository. It does not look in
your policy directory for them, even when one of yours has the same name.

A repository profile gets none of your attachments, whatever its name. A
policy that you attached to one session with `-n` does not apply to a
target.

All the policies of a target apply together, by the rules in
[Policies](policies.md). An allow rule in any one of them is a rule of the
run, and every route of every one of them applies. So a policy that you add
can give the agent more access. Plan the target again after you change its
policies.

A policy with an egress default or a route never leaves the run on the
shared network, even when you ask for `--network shared`. When two policies cannot apply
together, for example two routes with the same name, the plan opens with a
`REFUSED` row.

## Run a target

```bash
brig run --target dev
brig run --target dev -- --continue
brig run --target ci -f ci/Brigfile.ci
brig run
```

`brig run --target <name>` starts the sandbox of the target and runs its
agent. The project is the directory that holds the Brigfile. Brig mounts it
read-write at `/work/<name>`, and the agent starts there.

- The agent gets the target's `args`.
- Arguments after `--` replace the target's `args`, all of them.
  `brig run --target dev --` runs the agent with no arguments.
- `brig run` with no profile and no `--target` runs the Brigfile's
  `defaultTarget`. If the Brigfile names none, Brig lists the targets and
  exits `2`.
- In a directory with no Brigfile, `brig run` with no profile fails as it
  did before.

The line takes no profile and no project. A word before `--` that is not one
of Brig's flags is a usage error. The line takes `--session`, `-f`,
`--image`, `--mem`, `--cpus`, `--network`, `--offline`, `--publish`,
`--skills`, `-d`, `--json`, `--approve` and `--recreate`. It refuses `--home`
and `--no-project`.

Before the run boots anything, it asks for your approval.

## The approval

A Brigfile is the repository's. An agent in a sandbox can change the
Brigfile and every file it names. So reading a Brigfile allows nothing. A run
of a target boots only a configuration that you approved.

### What Brig asks

The first run of a target shows the plan of the run and asks. The plan has
the rows of `brig plan --target`, the arguments the agent gets, and the
digest of the approval:

```
brig: target dev in /Users/alex/code/demo asks to run with this configuration
brig:   ↳ the Brigfile and the files it names are the repository's, and the agent can change them
brig:   ↳ nothing runs until you approve it
brig:   ↳ the image is named by a tag, which can point at other bytes after you approve
TARGET       dev, from /Users/alex/code/demo/Brigfile
...
ARGS         "--continue"
APPROVAL     sha256:b8fce0d5...
brig: run target dev as shown above? [y/N]
```

Only `y` or `yes` approves. Any other answer, an empty line and a closed
input refuse. Brig stores a yes, and the next run with the same
configuration boots with no question.

When something changed since you approved, the run shows the plan again and
names each change:

```
brig: this differs from what you approved on 2026-11-13 17:01:32
brig:   ↳ the file .brig/egress.yaml (policy egress) changed
brig:   ↳ the arguments: "--resume", and were "--continue"
```

Brig refuses some runs before it asks, and you cannot approve them. Two
examples are two policies with routes of the same name, and a secret the run
needs that is not in your store.

### What an approval covers

- the Brigfile, and the profile and policy files of the target, by their
  sha256
- the profile, after the target's `overrides` and your settings
- each policy, with where it came from, the ones you attached to a built-in
  profile included
- the egress rules
- each route: where it sends requests, which secrets in your store it uses,
  and whether it hands the guest cloud keys
- the image reference, and whether a digest pins it
- the memory and the vCPUs
- the network that a flag or a setting asked for, and the network the
  sandbox gets
- the guest home, the project, and the directories that `--skills` copies
- the `BRIG_*` settings that take effect, by name and value, such as
  `BRIG_FORWARD_ENV`, `BRIG_IMAGE` and `BRIG_ENV_ARGV`
- each credential binding, and each host variable the run reads into the
  guest, by name
- the published ports
- the runtime and its backend
- the program the guest starts, and its arguments

A change to any of these asks again. A change to another file in the
project, such as your source code, does not.

The approval holds names, paths and digests. It holds no secret value.

### What an approval does not cover

- **An image named by a tag.** The tag can point at other bytes after you
  approve, and the prompt says so. To fix the bytes, name the image by digest
  in `overrides.image`. [Image verification](security.md#guest-images) still
  runs at every boot.
- **The value of a secret.** The plan names each secret that a route or a
  binding uses, and where each route sends it. If the secret is in your
  store, the run uses it. Read the route rows before you approve.
- **What the agent does** once it runs.

### Where approvals are kept

Brig keeps approvals in `approvals.json` in its state directory, `~/.brig`.
There is one approval for each session: one checkout, one target, one
session name. An approval of `dev` does not approve `review`, another session
of `dev`, or `dev` in another checkout of the same repository.

`brig rm --target` deletes the approval with the session. If Brig cannot read
`approvals.json`, nothing is approved. Brig does not write over that file,
and says so when it cannot store an approval.

### Approve ahead of time

With no terminal, and with `--json`, the run asks nothing. It shows the plan
on stderr, boots nothing, exits `8` and prints the digest:

```
brig: target ci is not approved to run as shown above, and there is no
terminal to ask on. To approve it, run the same line with --approve sha256:...
```

Read the plan. Then run the same line with `--approve <digest>`:

```bash
brig run --target ci --approve sha256:<digest>
```

Brig boots only when the digest is the digest of this run, and it stores the
approval. A digest of another run refuses with exit `8` and prints the
digest of this one.

The digest covers the path of the checkout, the flags and arguments on the
line, and your settings. Take it from a run on the same machine and in the
same directory, such as the first refused run in CI. Keep the digest out of
the repository, for example in a CI variable that only maintainers can
change. If the repository holds the digest, whoever changes the repository
can change the Brigfile and the digest together.

## Sessions of a target

Each target has a default session. `--session <name>` names another one:

```bash
brig plan --target dev                  # the default session of dev
brig plan --target dev --session fix    # the session fix of dev
```

A session name follows the rules of a label in a ref. It must already be its
own slug: lowercase letters, digits, `.`, `-` and `_`. See
[What a label can contain](sessions.md#what-a-label-can-contain).

Each session has its own sandbox and its own guest home. Both are named after
the project root, the target and the session, so two checkouts of one
repository never share a session. The guest home is
`~/.brig/homes/<sandbox name>`. Brig creates it and deletes it with the
sandbox. The sandbox keeps running after the agent exits, so the next run is
quick.

`brig run <profile>` in the same checkout is not a session of a target. It
keeps its own sandbox and guest home, and it does not read the Brigfile. It
says so in one line on stderr, except under `-q` and `--json`.

### Stop and remove a session

```bash
brig stop --target dev
brig rm --target dev --session fix
brig rm --target ci -f ci/Brigfile.ci --dry-run
```

These commands do not read the Brigfile. They find the session from the
project root, the target and the session name. So they work after the
Brigfile is deleted, and after the target is gone from it.

- `-f <path>` names the project: the directory of the path. The file does not
  have to exist.
- `brig rm --target` removes the sandbox and its guest home, as
  `brig rm <ref>` does for a guest home that Brig created. `--dry-run` works
  as it does there.
- If Brig has no record of the session and the runtime has no sandbox for
  it, the command exits `3`. The error names the project and the target.
- `brig rm --all` removes the sandboxes of targets too, with their guest
  homes.

`brig info`, `brig sh` and `brig logs` take a ref only. They refuse
`--target`.

### What `brig ls` shows

`brig ls` lists the sessions of targets in a second table, below the
sessions of profiles. Its columns are `TARGET`, `SESSION`, `SANDBOX`,
`STATE` and `PROJECT`.

| Row | Meaning | To remove it |
| --- | --- | --- |
| A target and a project | A session that Brig has a record of | `brig rm --target <name>` in that project |
| `STATE` is `unlisted` | Brig has a record of the session, and the runtime did not list its sandbox | `brig rm --target <name>` |
| `TARGET` is `-` and `PROJECT` is `unknown` | The runtime has a sandbox of a target that Brig has no record of | `brig rm --all` |

A session of a target has no ref, so `brig ls -q` prints no line for it. In
`brig ls --json`, each one has a `project` object. See
[`brig ls`](cli.md#brig-ls).

### A changed configuration

A sandbox keeps the configuration it was started with. When the Brigfile
changes what a target gets, the sandbox of a session that is already there
does not change. `brig plan --target` then says so in a `NOTE` row.

`brig run --target` does not start the new configuration on that sandbox. It
refuses, names what changed since your approval, and names two ways on:

```bash
brig run --target dev --recreate   # boots the new configuration on the same guest home
brig rm --target dev               # removes the session and its guest home
```

`--recreate` asks for your approval of the new configuration first. Then it
removes the sandbox and boots a new one. The guest home stays.

### The state directory

Brig keeps its record of the sessions of targets, and your approvals, in its
state directory, `~/.brig`. The project of a target is mounted read-write in
the guest. So `run`, `plan`, `stop` and `rm --target` refuse a project that
holds the state directory, and a project inside it, before they ask
anything. Set `BRIG_STATE_DIR` to a directory outside the project.

## What a repository profile may not set

A profile from `file:` or `inline:` comes from whoever wrote the repository.
It may describe the guest. It may not reach your host or weaken the sandbox:

| Field | Why it is refused |
| --- | --- |
| `runtimeBin` | It names a program on your host for Brig to run |
| `hostConfigDir` | It names a host directory that Brig copies into the guest |
| `projectPaths` | It names host directories that Brig copies into the guest |
| `sources`, `from`, `service`, `path`, `var`, `hint`, `field` or `expiryField` on a secret | It says where on your host a secret's value comes from. A repository may name a secret, but not its source |
| `ref: env.<name>` | It reads a variable from your environment into the guest |
| a `hostmount` volume | It shares a host directory with the guest. A repository profile may declare `tmpfs` volumes |
| `network: shared` | It puts the sandbox on a network it shares with other sandboxes. A repository profile may ask for `isolated` or `offline` |
| `hypervisor` | It names a hypervisor on your host, and that decides which network the sandbox can get |
| an `image` that starts with `-` | The runtime would read it as a flag |

A profile or a policy from `file:` or `inline:` also may not hold a control
character in any string, a newline and a tab included. Its strings appear in
the plan, and a control character could change what your terminal shows.

A profile Brig ships through `builtin:` is not limited this way.

## Where Brig looks

Inside a Git checkout, Brig reads the `Brigfile` at the top of the
worktree: the nearest directory that holds a `.git` entry. It does not read
one in a subdirectory or in a directory above. Outside Git, Brig reads the
`Brigfile` in the current directory only.

`-f <path>` names a Brigfile instead. Its directory is then the project
root, and `file:` paths resolve from there.

```bash
brig target ls -f ci/Brigfile.ci
```

## The commands

| Command | What it does |
| --- | --- |
| [`brig target ls`](cli.md#brig-target) | Lists the targets, marks the default, and names each profile and its source |
| [`brig config`](cli.md#brig-config) | Shows the project, or one target resolved with `--target <name>` |
| [`brig init`](cli.md#brig-init) | Writes a starter Brigfile |
| [`brig plan --target`](cli.md#brig-plan) | Shows the permissions that a run of one target would get |
| [`brig run --target`](cli.md#run-a-brigfile-target) | Runs a target, once you approve it |
| [`brig stop --target`](cli.md#brig-stop) | Stops a session of a target |
| [`brig rm --target`](cli.md#brig-rm) | Removes a session of a target, with its guest home |

The first three commands read files and write nothing, except the one file
`brig init` creates. They start no sandbox, open no secret and need no
runtime. `target ls` and `config` take `--json`.

`brig plan --target` also writes nothing and opens no secret. Like
`brig plan <profile>`, it looks for a runtime, asks the runtime about the
sandbox, and lists the names in your secret store. `brig stop --target` and
`brig rm --target` need a runtime, and do not read the Brigfile.

`brig run --target` reads the Brigfile and the files it names once. What it
boots comes from that one read, so a change made while it asks is not what
boots. It reads your secrets only after you approve.

With no Brigfile, `target ls`, `config` and `plan --target` exit `3` and say
where they looked. A Brigfile that does not load exits `1`. See
[Exit codes](cli.md#exit-codes).

## Stability

`brig.sh/v1alpha1` is an alpha format. It carries no compatibility promise:
a field can be added, renamed or removed in any release. See
[Stability](stability.md).
