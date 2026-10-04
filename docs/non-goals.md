# What Brig will not do

Brig delegates every mechanical operation to a runtime it does not own. It
keeps its dependencies to four and needs no account. It adds only the
things that the runtime has no concept of. The limits below are written
decisions, so a proposal to cross one has a place to start.

Each item is a decision for the next twelve months, through August 2027.
Each item names the change in circumstances that reopens it. If you think
that a trigger is met, say which one in an issue.

## A container runtime, or a VMM

Brig boots nothing itself. [Runtimes](runtimes.md) says what does, and
where the decisions of Brig end and the decisions of the runtime start.

To own the boot path is to own Virtualization.framework, a kernel command
line, an image store, a snapshotter, and the vulnerability surface of all of
them. Brig exists to handle your credentials carefully, with four direct
dependencies. The README section "How it works" lists the four things that
Brig adds on top:

- the guest home and project mounts
- credentials forwarded by name
- a billing denylist
- image verification

A runtime has no concept of any of them. Everything else in the boot path
already works.

**Reopens when** a runtime that Brig can drive refuses upstream to expose
something that the guest home promise or the credential promise needs. A
subprocess must also be unable to get it. Even then, the first move is a patch to
that runtime, not a new runtime.

## A rules language for policy

A profile field that the runtime enforces is in scope. For example,
`network: offline` is data: Brig translates it into the flag of the runtime.
The job of Brig ends at the translation, and the runtime makes the
guarantee.

An egress policy has the same shape, one step up. It is a document of
`allow` and `deny` rules that Brig hands to the gateway that enforces them.
Brig refuses the policy where nothing can enforce it
([Policies](policies.md)). A missing field of that kind is in scope.

A language is out of scope: conditions, matchers, precedence rules, and an
evaluator that lives in Brig. People will believe that a decision Brig
evaluates is enforced. Enforcement sits one layer down, and Brig can only
ask for it.

**Reopens when** profile settings need to combine conditionally in a way
that plain fields cannot express. Every condition in the language must also
have an enforcement point in the runtime. A rule with no enforcement point
stays out, however much people want it.

## A TLS-terminating host proxy

The sandboxes of Docker keep credentials out of the guest: a host-side
proxy rewrites the auth headers. Brig forwards the credential instead.
[What is still exposed](security.md#what-is-still-exposed) states what that
costs.

A proxy covers only proxied HTTP. It does nothing for these operations,
which are the ones an agent performs:

- `git push`
- a vendor CLI that refreshes its token
- an MCP server that holds its own connection

The proxy also has a cost on the host. A process that terminates TLS for the
guest holds every credential in cleartext, on the host, for the life of the
sandbox. It also needs a CA that the guest is made to trust. That process is
a new and attractive target. In exchange, it narrows an exposure that Brig
already limits to a narrow blast radius: one guest home, one fine-grained
token.

**Reopens when** one of these is true:

- The agents that people run under Brig have a credential surface that is
  entirely HTTP a proxy can see.
- A runtime offers a credential broker with a per-request hook, so the
  plaintext lives somewhere other than a Brig process.

## Remote or Kubernetes operation

Brig runs on the machine in front of you. Three of its properties depend on
that:

- The guest home is a live host directory, not a copy. That is why Brig has
  no `cp` verb.
- Brig resolves credentials from your own keychain per invocation.
- On the two profiles that declare a tmpfs, `claude-code` and
  `claude-desktop`, an in-sandbox login lives in guest memory and ends with
  `brig stop`. On the other profiles, the login is on the persisted guest
  home, like everything else there.

On a remote host, each property becomes a problem:

- The guest home becomes a synchronisation problem.
- The credential path becomes a transport with its own threat model.
- A login meant to stay in memory sits on a machine that you are not in
  front of.

Kubernetes adds a controller, a custom resource, an image pull secret story,
and a scheduler on top. For remote use today, `ssh` to the host and run Brig
there.

**Reopens when** it becomes the common case to run the agent on a different
machine from the one you edit on. It also needs a design that keeps the
credential on the machine of the operator and does not copy it to the
remote host.

## A hosted control plane or account

There is nothing to sign in to. Every piece of state is on your disk:

| State | Location |
| --- | --- |
| Profiles | `~/.config/brig` |
| Secrets | Your login keychain or keyring |
| Guest homes | `~/.brig/homes` |
| Sessions | The inventory of `brigd` |

Brig registers nowhere. [Telemetry](telemetry.md) lists the anonymous usage
events it sends, and how to turn them off.

Nothing we run can be down while you try to boot a sandbox. An account also
widens the security page. The threat model of Brig then has to include our
servers, our operators and our outages. Today it has to mention none of
them.

**Reopens when** a feature that people want is impossible without a shared
service. Even then, it ships as a separate opt-in service and not as a
requirement. Brig without an account keeps doing everything it does today,
permanently.

## SDKs in several languages

Each language library adds a release train, a dependency set, and a chance
to drift behind the CLI. All of that wraps a process that the caller can spawn directly. The short dependency
list in `CONTRIBUTING.md` exists for the tool. The same reasoning applies to
what we ask users to link into their own programs.

The commitment is a CLI that does not need a wrapper. It gives stable verbs,
exit codes, and human output on stdout with diagnostics on stderr. A
`--json` mode serves a program that reads the output. Today that mode
covers:

- The read verbs: `ls`, `info`, `plan`, `agent ls`, `agent show`,
  `agent export`, `agent new`, `policy show`, `secret ls`, `doctor`,
  `version` and the `network` verbs.
- `run` and `sh`, which under `--json` report the exit status of the agent
  on one line.
- `env`, the deprecated spelling of `info`.

More verbs get `--json` as callers need them. Open an issue to ask for one.

For lifecycle control, there is an interface with no library attached.
`brigd` speaks line-delimited JSON over a unix socket. See
[brigd](brigd.md).

**Reopens when** callers reimplement the same non-trivial logic in several
languages against the output of Brig. The cause must be something that a
`--json` mode cannot fix. An example is a protocol that needs a handshake that a
shell caller cannot perform.

## Dashboard or TUI before CLI

The non-goal is the order. `brig ls`, `brig info` and `brig agent ls` show
what exists, what Brig will forward, and what each profile refuses. While
any of that is missing from the CLI, a screen that shows it comes too
early.

A live interface must also stay in the middle of a session, and Brig does
not. `brig sh` replaces itself with the runtime, so `^C` and the exit status
belong to the agent. [docs/security.md](security.md) explains that cost and
accepts it.

**Reopens when** the CLI covers the whole surface and someone shows a task
that is hard to read as text. An example is watching several sandboxes at
once. That interface must then be a client of the `brigd` protocol, so that
nothing becomes reachable only through a screen.

## Containers inside the guest

Brig refuses nested containers, and a Docker daemon inside the guest, for
lack of demand and not on principle.

Containers inside the guest need either nested virtualization or a
privileged guest. They also need a second image store on host disk, inside
the guest home. That complicates the model the tool is built around: the
agent can reach a small, named set of host directories. Nobody has yet
shown that the cost is worth paying.

**Reopens when** people report concrete workflows that need it. Examples are
a repo whose tests bring up containers, and a compose file that the agent is
meant to run. When those workflows exist, and a runtime under Brig supports
them without a privileged guest, this becomes an ordinary feature
discussion.

## GPU scheduling

The method to pass a device into a microVM is specific to each hypervisor
and each driver. To divide a small number of devices between sandboxes,
Brig must arbitrate a resource that it does not own. Brig delegates that
category of work.

GPU scheduling is also aimed at the wrong place. The agents that Brig runs
are CLIs that call a model over the network, so the accelerator that matters
is not in your machine.

**Reopens when** an agent that people run under Brig does local inference as
its normal mode. Even then, the answer is a profile field that names a
device, handed to the runtime the way `mem` and `cpus` are. Scheduling
across sandboxes stays out.

## Windows

The guest is Linux on every host. This non-goal is about the host. A Windows
host is three new things at once:

- A runtime path that Brig does not drive today.
- A secret backend with no relationship to the keychain behaviour that
  `docs/security.md` documents in detail.
- A filesystem whose case and symlink semantics differ from the ones the
  guest home safety code was written and tested against.

That safety code refuses a planted symlink through an `os.Root`. It is one
of the two promises, so nobody ports it on the assumption that it still
holds.

The route that exists is to run Brig on Linux. That includes a Linux
environment hosted on a Windows machine, where the microVM path needs nested
virtualization to be available.

**Reopens when** there is a Windows-native runtime that Brig can drive. It
also needs someone willing to own the secret backend and the guest home path
tests on that platform. That person must keep owning them as they change.

## A plugin system

The short dependency list exists to prevent third-party code in the process
that resolves your credentials. A plugin system loads that code. A plugin
API also turns internal interfaces into a contract that we then cannot
change.

Brig is extensible in three ways without one:

- **Profiles are data.** Any Linux CLI in an OCI image runs under Brig with
  a YAML file and no code.
- **Secrets come from any backend.** Anything that can put a value into the
  store of Brig or its environment is a usable source.
- **`brigd` speaks a documented protocol** to whatever wants to drive
  sessions.

Where Brig uses outside code, it runs it as a subprocess with a defined
interface: `cosign`, `oras`, `security`.

**Reopens when** a kind of extension appears that is none of those three and
people keep asking for it. The answer then is another subprocess with a
defined protocol, in the shape of `cosign` and `oras`. It is not code loaded
into the address space of Brig.

## Proposing one of these anyway

Open an issue with the `enhancement` label. Name the item. Say which trigger
you think is met, and what changed. If an item here is wrong, and not only
early, open an issue for that too.
