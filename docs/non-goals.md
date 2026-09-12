# What Brig will not do

Brig's shape comes from what it refuses. Brig delegates every mechanical
operation to a runtime it does not own. It keeps its dependencies to
three, needs no account, and adds only the things the layer underneath
has no concept of. None of that was written down as a boundary, so every
proposal to cross one arrived as a fresh argument with no prior answer.
This page is the prior answer.

Each item below is a decision for the next twelve months, through August
2027. Each one names the change in circumstances that reopens it. A
non-goal with no reopening condition is a grudge, not a decision. If you
think a trigger here has been met, say which one in an issue. The
discussion then starts from there instead of from first principles.

This is not a list of bad software. Most of these items are good software
somewhere else.

## A container runtime, or a VMM

Brig boots nothing itself. See [runtimes.md](runtimes.md) for what does,
and for the boundary between what Brig decides and what the runtime
decides.

Owning the boot path means owning Virtualization.framework, a kernel
command line, an image store, a snapshotter, and the vulnerability
surface of all of it. Brig's reason to exist is that it handles your
credentials carefully, with three direct dependencies. The four things
Brig adds on top, in the README's "How it works," are the four a runtime
has no concept of. Everything else in the boot path already works.

**Reopens when** a runtime Brig can drive refuses upstream to expose
something the guest home or credential promise needs. Shelling out cannot
get it either. Even then the first move is a patch to that runtime, not a
runtime of our own.

## A policy engine with its own rules language

There is a line here, worth being exact about. A profile field the
runtime enforces is fine: `network: offline` translated into the
runtime's own flag is data. Brig's job ends at the translation, and the
guarantee is the runtime's to make. An egress policy is the same shape
one step up: a document of `allow` and `deny` rules handed to the gateway
that enforces them. It is refused where nothing can enforce it
([policies.md](policies.md)). A missing field of that kind is in scope.
What is out of scope is a language: conditions, matchers, precedence
rules, and an evaluator that lives in Brig. A decision Brig evaluates is
one people will believe is enforced. In fact, enforcement sits one
layer down, and Brig can only ask for it.

**Reopens when** profile settings need to combine conditionally in a way
plain fields cannot express. Every condition in the language must also
have an enforcement point in the runtime underneath it. A rule with no
enforcement point stays out, no matter how much it is wanted.

## A host-side proxy that terminates TLS

Docker's sandboxes keep credentials out of the guest by rewriting auth
headers in a host-side proxy. Brig forwards the credential instead.
`docs/security.md` says plainly what that costs. The reason is that a
proxy only covers proxied HTTP. It does nothing for `git push`, for a
vendor CLI refreshing its own token, or for an MCP server holding its own
connection. Those are the operations an agent actually performs.

The proxy is not free on the host side either. A process that terminates
TLS for the guest holds every credential in cleartext, on the host, for
the life of the sandbox. It also needs a CA the guest is made to trust.
That is a new and attractive target. In exchange, it narrows an exposure
Brig already answers a different way, with a narrow blast radius: one
guest home, one fine-grained token.

**Reopens when** the agents people run under Brig have a credential
surface that is entirely HTTP a proxy can see. It also reopens when a
runtime offers a credential broker with a per-request hook, so the
plaintext lives somewhere other than a Brig process.

## Remote or Kubernetes operation

Brig runs on the machine in front of you, and three of its properties
depend on that. The guest home is a live host directory, not a copy,
which is why there is no `cp` verb. Credentials are resolved from your
own keychain per invocation. On the two profiles that declare a tmpfs,
`claude-code` and `claude-desktop`, an in-sandbox login lives in guest
memory and dies with `brig stop`. On the rest it is already on the
persisted guest home, like everything else there.

Point Brig at a remote host, and the guest home becomes a synchronisation
problem. The credential path also becomes a transport with its own
threat model. A login that was meant to stay in memory now sits on a machine
you are not in front of. Kubernetes adds a controller, a custom resource,
an image pull secret story, and a scheduler on top of all that. The
remote story that works today is the boring one: `ssh` to the host and
run Brig there.

**Reopens when** running the agent on a different machine from the one
you edit on becomes the common case rather than an occasional one. It
also needs a design that keeps the credential on the operator's machine,
rather than copying it to the remote host.

## A hosted control plane, or any account

There is nothing to sign in to, and that is a feature with a price we are
willing to pay. Every piece of state is on your disk: profiles in
`~/.config/brig`, secrets in your login keychain, guest homes in `~/brig`,
sessions in `brigd`'s inventory. Nothing registers, and nothing phones
home. Nothing we run can be down while you are trying to boot a sandbox.
An account also widens the security page. Brig's threat model then has
to include our servers, our operators and our outages, none of which it
has to mention today.

**Reopens when** a feature people want turns out to be impossible without
a shared service. Even then it ships as a separate opt-in service rather
than as a requirement. Brig without an account keeps doing everything it
does today, permanently. That part is not up for review in twelve months
or in sixty.

## SDKs in several languages

A library per language is a release train per language, a dependency set
per language, and a chance per language to drift behind the CLI. All of
that to wrap a process the caller can spawn directly. The short
dependency list in `CONTRIBUTING.md` exists for the tool itself. The same
reasoning applies to what we ask users to link into their own programs.

The commitment instead is a CLI disciplined enough not to need wrapping.
It gives stable verbs, exit codes, and human output on stdout with
diagnostics on stderr. A `--json` mode covers it wherever a program is
the reader rather than a person. Today that mode covers the read verbs:
`ls`, `info`, `agent ls`, `agent show`, `agent export`, `agent new`,
`policy show`, `secret ls` and `doctor`. It also covers `run` and `sh`,
which under `--json` report the agent's exit status on one line. `env`,
the deprecated spelling of `info`, takes it too. More verbs get it as
callers need them, and asking for one is a small issue rather than an
argument. For
lifecycle control there is already an interface with no library
attached: `brigd` speaks line-delimited JSON over a unix socket and is
documented in [brigd.md](brigd.md).

**Reopens when** callers are reimplementing the same non-trivial logic in
several languages against Brig's output, and the cause is something a
`--json` mode cannot fix. An example is a protocol that needs a
handshake a shell caller cannot perform.

## A dashboard or a TUI ahead of the CLI

The refusal is in the last three words. `brig ls`, `brig info` and
`brig agent ls` are how you see what exists, what will be forwarded, and
what each profile refuses. While any of that is missing from the CLI,
adding a screen that displays it is building the second floor first. A
live interface also has to stay in the middle of something, and Brig
deliberately does not. `brig sh` replaces itself with the runtime, so
`^C` and the exit status are the agent's own, a cost
[docs/security.md](security.md) explains and accepts.

**Reopens when** the CLI covers the whole surface and someone shows a
task that is genuinely hard to read as text. An example is watching
several sandboxes at once. Such a thing must then be a client of
`brigd`'s protocol, so that nothing becomes reachable only through a
screen.

## Nested containers, or a Docker daemon inside the guest

This one is refused for want of demand rather than on principle. Running
containers inside the guest needs either nested virtualization or a
privileged guest. It also needs a second image store living on host
disk inside the guest home. That complicates the model the whole tool
is built around: a small, named set of host directories is what the
agent can reach. That is a real cost, and nobody has yet shown it is
worth paying.

**Reopens when** people report the workflows that need it, concretely.
Examples are a repo whose tests bring up containers, or a compose file
the agent is meant to run. Once those exist, and a runtime under Brig
supports them without a privileged guest, this becomes an ordinary
feature discussion rather than a non-goal.

## GPU scheduling

Passing a device into a microVM is specific to each hypervisor and each
driver. Dividing a small number of devices between sandboxes means Brig
arbitrating a resource it does not own. That is the exact category of
work it delegates. It is also aimed at the wrong place. The agents Brig
runs are CLIs that call a model over the network, so the accelerator that
matters is not in your machine.

**Reopens when** an agent people run under Brig does local inference as
its normal mode. Even then the answer is a profile field naming a device,
handed to the runtime the way `mem` and `cpus` are. Scheduling across
sandboxes stays out.

## Windows

The guest is Linux either way. This is about the host. A Windows host is
three new things at once. It has a runtime path Brig does not drive
today. It has a secret backend with no relationship to the keychain
behaviour `docs/security.md` documents in detail. And it has a filesystem
whose case and symlink semantics differ from the ones the guest home
safety code was written and tested against. That code refuses a planted
symlink through an `os.Root`. It is one of the two promises, so it does
not get ported on the assumption it still holds. The route that exists is
to run Brig on Linux. That includes a Linux environment hosted on a
Windows machine, where the microVM path needs nested virtualization to be
available.

**Reopens when** there is a Windows-native runtime Brig can drive. It
also needs someone willing to own the secret backend and the guest home
path tests on that platform, not once but as they change.

## A plugin system

Loading third-party code into the process that resolves your credentials
is the thing the short dependency list exists to prevent. A plugin API
also turns internal interfaces into a contract we then cannot change.
Brig is already extensible three ways that do not require it. Profiles
are data, so any Linux CLI in an OCI image runs under Brig with a YAML
file and no code at all. Secrets come from any backend you like, since
anything that can put a value into Brig's store or its environment is a
usable source. And `brigd` speaks a documented protocol to whatever wants
to drive sessions. Where Brig does use outside code, it does so as a
subprocess with a defined interface: `cosign`, `oras`, `security`.

**Reopens when** a kind of extension appears that is none of those three
and keeps being asked for. The answer then is another subprocess with a
defined protocol, in the shape of `cosign` and `oras`, rather than code
loaded into Brig's address space.

## Proposing one of these anyway

Open an issue with the `enhancement` label, name the item, and say which
trigger you think has been met and what changed. That is a much shorter
conversation than the general case, which is the point of writing the
list down. If an item here turns out to be wrong rather than merely
early, that is worth an issue too. This page is a decision, and decisions
get revisited.
