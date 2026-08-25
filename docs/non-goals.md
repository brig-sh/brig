# What brig will not do

brig's shape comes from what it refuses. It delegates every mechanical
operation to a runtime it does not own, keeps one direct dependency, needs no
account, and adds only the things the layer underneath has no concept of. None
of that was written down as a boundary, so every proposal to cross one arrived
as a fresh argument with no prior answer. This page is the prior answer.

Each item below is a decision for the next twelve months, so through August
2027, and each carries the change in circumstances that would reopen it. A
non-goal with no reopening condition is a grudge rather than a decision, so if
you think a trigger here has been met, say which one in an issue and the
discussion starts from there instead of from first principles.

This is not a list of things that would be bad software. Most of them are good
software somewhere else.

## A container runtime, or a VMM

brig boots nothing itself. `hull` boots the microVM on macOS, `nerdctl` over
containerd does it on Linux, and brig's own work begins after the sandbox is
running. Owning the boot path would mean owning Virtualization.framework, a
kernel command line, an image store, a snapshotter and the vulnerability
surface of all of it, in a tool whose reason to exist is that it handles your
credentials carefully with one direct dependency. The four things brig adds on
top, in the README's "What brig is", are the four a runtime has no concept of.
Everything else in the boot path already works.

**Reopens when** a runtime brig can drive refuses upstream to expose something
the workspace or credential promise needs, and there is no way to get it by
shelling out. Even then the first move is a patch to that runtime, not a
runtime of our own.

## A policy engine with its own rules language

There is a line here, and it is worth being exact about where it falls. A
profile field the runtime enforces is fine: `network: none` translated into
the runtime's own flag is data, brig's job ends at the translation, and the
guarantee is the runtime's to make. The README already lists sbx's
`--publish` and `--deny-network` as "not yet" rather than never, and
`docs/security.md` says there is no per-sandbox network policy yet. Those are
missing fields, and fields are in scope. What is out of scope is a language:
conditions, matchers, precedence rules and an evaluator that lives in brig. A
decision brig evaluates is one people will believe is enforced, when in fact
enforcement sits one layer down and brig can only ask for it.

**Reopens when** profile settings genuinely need to combine conditionally in a
way plain fields cannot express, *and* every condition in the language has an
enforcement point in the runtime underneath it. A rule with no enforcement
point stays out however much it is wanted.

## A host-side proxy that terminates TLS

Docker's sandboxes keep credentials out of the guest by rewriting auth headers
in a host-side proxy. brig forwards the credential instead, and
`docs/security.md` says plainly what that costs. The reason is that a proxy
only covers proxied HTTP: it does nothing for `git push`, for a vendor CLI
refreshing its own token, or for an MCP server holding its own connection, and
those are the operations an agent actually performs. The second reason is that
the proxy is not free on the host side either. A process that terminates TLS
for the guest holds every credential in cleartext, on the host, for the life of
the sandbox, and needs a CA the guest is made to trust. That is a new and
attractive target in exchange for narrowing an exposure brig already answers a
different way, with a narrow blast radius: one workspace, one fine-grained
token.

**Reopens when** the agents people run under brig have a credential surface
that is entirely HTTP a proxy can see, or a runtime offers a credential broker
with a per-request hook so the plaintext lives somewhere other than a brig
process.

## Remote or Kubernetes operation

brig runs on the machine in front of you, and three of its properties depend on
that. The workspace is a live host directory, not a copy, which is why there is
no `cp` verb. Credentials are resolved from your own keychain per invocation.
An in-sandbox login lives in guest memory and dies with `brig stop`. Point brig
at a remote host and the workspace becomes a synchronisation problem, the
credential path becomes a transport with its own threat model, and the login
that was deliberately memory-only is now sitting on a machine you are not in
front of. Kubernetes adds a controller, a custom resource, an image pull secret
story and a scheduler on top of all that. The remote story that works today is
the boring one: `ssh` to the host and run brig there.

**Reopens when** running the agent on a different machine from the one you edit
on becomes the common case rather than an occasional one, and a design exists
that keeps the credential on the operator's machine rather than copying it to
the remote host.

## A hosted control plane, or any account

There is nothing to sign in to, and that is a feature with a price we are
willing to pay. Every piece of state is on your disk: profiles in
`~/.config/brig`, secrets in your login keychain, workspaces in `~/brig`,
sessions in `brigd`'s inventory. Nothing registers, nothing phones home,
nothing we run can be down while you are trying to boot a sandbox. An account
would also widen the security page: brig's threat model would have to include
our servers, our operators and our outages, and today it does not have to
mention them at all.

**Reopens when** a feature people want turns out to be impossible without a
shared service, and even then it ships as a separate opt-in service rather than
as a requirement. brig without an account keeps doing everything it does today,
permanently. That part is not up for review in twelve months or in sixty.

## SDKs in several languages

A library per language is a release train per language, a dependency set per
language, and a chance per language to drift behind the CLI, all to wrap a
process the caller could have spawned. The single-dependency rule in
`CONTRIBUTING.md` exists for the tool itself, and the same reasoning applies to
what we ask users to link into their programs. The commitment instead is a CLI
disciplined enough not to need wrapping: stable verbs and exit codes, human
output on stdout, diagnostics on stderr, and a `--json` mode wherever a program
is the reader rather than a person. Today that mode exists on
`brig profile export`; more verbs get it as callers need them, and asking for
one is a small issue rather than an argument. For lifecycle control there is
already an interface with no library attached: `brigd` speaks line-delimited
JSON over a unix socket and is documented in [brigd.md](brigd.md).

**Reopens when** callers are reimplementing the same non-trivial logic in
several languages against brig's output, and the cause is something a `--json`
mode cannot fix, such as a protocol that needs a handshake a shell caller
cannot perform.

## A dashboard or a TUI ahead of the CLI

The refusal is in the last three words. `brig ls`, `brig env` and
`brig profiles` are how you see what exists, what would be forwarded and what
each profile refuses, and while any of that is missing from the CLI, adding a
screen that displays it is building the second floor first. A live interface
also has to stay in the middle of something, and brig deliberately does not:
`brig exec` replaces itself with the runtime so `^C` and the exit status are
the agent's own, which `docs/security.md` explains and accepts the cost of.

**Reopens when** the CLI covers the whole surface and someone shows a task that
is genuinely hard to read as text, such as watching several sandboxes at once.
Such a thing should then be a client of `brigd`'s protocol, so that nothing
becomes reachable only through a screen.

## Nested containers, or a Docker daemon inside the guest

This one is refused for want of demand rather than on principle. Running
containers inside the guest needs either nested virtualization or a privileged
guest, plus a second image store living on host disk inside the workspace, and
it complicates the sentence the whole tool is built around: one directory is
the agent's whole world. That is a real cost, and nobody has yet shown it is
worth paying.

**Reopens when** people report the workflows that need it, concretely: a repo
whose tests bring up containers, a compose file the agent is meant to run. Once
those exist and a runtime under brig supports them without a privileged guest,
this becomes an ordinary feature discussion rather than a non-goal.

## GPU scheduling

Passing a device into a microVM is specific to each hypervisor and each driver,
and dividing a small number of devices between sandboxes means brig arbitrating
a resource it does not own, which is the exact category of work it delegates.
It is also aimed at the wrong place: the agents brig runs are CLIs that call a
model over the network, so the accelerator that matters is not in your machine.

**Reopens when** an agent people run under brig does local inference as its
normal mode. Even then the answer is a profile field naming a device, handed to
the runtime the way `mem` and `cpus` are. Scheduling across sandboxes stays
out.

## Windows

The guest is Linux either way. This is about the host, and a Windows host is
three new things at once: a runtime path brig does not drive today, a secret
backend with no relationship to the keychain behaviour `docs/security.md`
documents in detail, and a filesystem whose case and symlink semantics differ
from the ones the workspace safety code was written and tested against. That
code refuses a planted symlink through an `os.Root`, and it is one of the two
promises, so it does not get ported on the assumption it still holds. The route
that exists is to run brig on Linux, including a Linux environment hosted on a
Windows machine, where the microVM path needs nested virtualization to be
available.

**Reopens when** there is a Windows-native runtime brig can drive, and someone
willing to own the secret backend and the workspace path tests on that
platform, not once but as they change.

## A plugin system

Loading third-party code into the process that resolves your credentials is
the thing the single-dependency rule exists to prevent, and a plugin API also
turns internal interfaces into a contract we then cannot change. brig is
already extensible three ways that do not require it. Profiles are data, so
any Linux CLI in an OCI image runs under brig with a YAML file and no code at
all. Secrets come from any backend you like, since anything that can put a
value into brig's store or its environment is a usable source. And `brigd`
speaks a documented protocol to whatever wants to drive sessions. Where brig
does use outside code it does so as a subprocess with a defined interface:
`cosign`, `oras`, `security`.

**Reopens when** a kind of extension appears that is none of those three and
keeps being asked for. The answer then is another subprocess with a defined
protocol, in the shape of `cosign` and `oras`, rather than code loaded into
brig's address space.

## Proposing one of these anyway

Open an issue with the `enhancement` label, name the item, and say which
trigger you think has been met and what changed. That is a much shorter
conversation than the general case, which is the point of writing the list
down. If an item here turns out to be wrong rather than merely early, that is
worth an issue too: this page is a decision, and decisions get revisited.
