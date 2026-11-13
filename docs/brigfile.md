# The Brigfile

A Brigfile is a file at the top of a repository that says how to run the
repository under Brig. It names profiles and policies by alias, and declares
one or more targets. A target pairs one profile with its policies, a size
and default arguments.

You can write a Brigfile, check it, see what each target resolves to, and
plan a run of a target. `brig run` does not read a Brigfile yet. A run still
uses the agent and the flags on its own command line.

## Start one

```bash
brig init                  # writes a Brigfile at the top of the worktree
brig target ls             # lists its targets
brig config --target dev   # shows what dev resolves to
brig plan --target dev     # shows what a run of dev would get
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
| `args` | no | The arguments the agent gets when the run gives none |

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

`brig plan --target` takes the flags `--image`, `--mem`, `--cpus`,
`--network`, `--offline`, `--publish`, `--skills` and `--json`. It takes no
profile and no project, and it refuses `--home` and `--no-project`.
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

`BRIG_NAME` and `BRIG_WORKSPACE` do not apply to a target. Each target has
its own sandbox and its own guest home, named after the project and the
target. The sandbox name of a target starts with `brig--`.

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

The first three commands read files and write nothing, except the one file
`brig init` creates. They start no sandbox, open no secret and need no
runtime. `target ls` and `config` take `--json`.

`brig plan --target` also writes nothing and opens no secret. Like
`brig plan <profile>`, it looks for a runtime, asks the runtime about the
sandbox, and lists the names in your secret store.

With no Brigfile, `target ls`, `config` and `plan --target` exit `3` and say
where they looked. A Brigfile that does not load exits `1`. See
[Exit codes](cli.md#exit-codes).

## Stability

`brig.sh/v1alpha1` is an alpha format. It carries no compatibility promise:
a field can be added, renamed or removed in any release. See
[Stability](stability.md).
