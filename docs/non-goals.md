# What Brig will not do

Brig delegates every mechanical operation to a runtime it does not own. It
keeps its dependencies to four and needs no account to run. It adds only the
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

The credential broker of [routes](policies.md#routes) is not this proxy. It
terminates no TLS that the guest opens to an upstream. The guest sends its
request to the broker's own endpoint, `brig.internal`, with a CA that signs
only that name. The broker then opens its own TLS connection to the
upstream, verified with the system roots. A route covers a tool that is
pointed at it, and nothing else.

**Reopens when** one of these is true:

- The agents that people run under Brig have a credential surface that is
  entirely HTTP a proxy can see.
- A runtime offers a credential broker with a per-request hook, so the
  plaintext lives somewhere other than a Brig process.

## A required account

Brig Enterprise has an optional NOFire AI account. This replaces the earlier
non-goal of no hosted control plane or account.
[`brig login`](cli-enterprise.md#brig-login) signs you in. Nothing in Brig
needs it, and one optional feature uses it: [session
recording](cli-enterprise.md#session-recording), which your organization
turns on at the deployment. A run whose input or output is not a terminal, or
whose stdin and stdout are different terminals, is not recorded and runs
exactly as it does without recording, and so is a run on a runtime other than
hull. Brig Community has no account. `brig run`, `brig sh` and every other verb
work without an account.

Every piece of state a run needs is still on your disk:

| State | Location |
| --- | --- |
| Profiles | `~/.config/brig` |
| Secrets | Your login keychain or keyring |
| Guest homes | `~/.brig/homes` |
| Sessions | The inventory of `brigd` |

The login token is stored in your keychain under its own service, and a
sandbox never receives it. [The NOFire AI login
token](security.md#the-nofire-ai-login-token) lists the limits.
[Telemetry](telemetry.md) stays anonymous when you are logged in.

A run still boots with no network and no account. A signed-in Brig Enterprise
run asks the deployment once, with a 3 second timeout, whether to record it,
and goes on unrecorded on any failure. So an outage on our side cannot stop a
sandbox from booting. The account adds our servers to the threat model for
`brig login` and `brig logout`, and for every signed-in run, because a signed-in
run asks the deployment whether to record it even when the answer is no.
[Session recording](security.md#session-recording) lists what that sends.

**Reopens when** a feature requires the account for a run, and not only for a
service beside it. A required account is still a non-goal. Brig without an
account keeps doing everything it does today, permanently.

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
belong to the agent. A recorded run of Brig Enterprise is the one exception:
Brig stays the parent to record the output, and passes the terminal and the
exit status through. [docs/security.md](security.md) explains that cost and
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

## Reopened

These items were decisions on this page, and work on them is now in
progress. Each issue carries the design and the constraints that the item
named.

| Item | Issue |
| --- | --- |
| Remote operation | [#482](https://github.com/brig-sh/brig/issues/482) |
| Kubernetes operation | [#483](https://github.com/brig-sh/brig/issues/483) |

Two related pieces of work are in progress too: an OpenShell compute driver
that boots Brig sandboxes
([#484](https://github.com/brig-sh/brig/issues/484)), and a compose stack
run as a set of sandboxes
([#485](https://github.com/brig-sh/brig/issues/485)).

Until remote operation ships, `ssh` to the host and run Brig there.

A rules language for policy is reopened too. A policy over a sequence of
agent actions needs conditions that plain fields cannot express. Examples
are an approval before a credential is delivered, and a limit on how often
that happens. The design is in review. Two rules apply:

- Every condition in a policy names an enforcement point outside the guest,
  where the action is stopped before it takes effect. A rule with no
  enforcement point stays out.
- Brig refuses a policy on a run path that cannot enforce it, as it does for
  an egress policy today ([Policies](policies.md)).

## Proposing one of these anyway

Open an issue with the `enhancement` label. Name the item. Say which trigger
you think is met, and what changed. If an item here is wrong, and not only
early, open an issue for that too.
