# Networking and egress policy

A sandbox runs under one of three network postures. You can bind an
egress policy to a profile, or to one session, on top of that.

An egress policy is enforced only on hull's `hvi` backend, which needs macOS
15 or newer and a hull newer than 0.1.0-rc21. Every other runtime, including
every Linux runtime, refuses to boot a policy it cannot enforce. The one
exception is `--network offline`: it reaches no network at all, so it
satisfies any egress rule and is never refused. See
[Where a policy is enforced, and where it is not](#where-a-policy-is-enforced-and-where-it-is-not).

## Network postures

Every sandbox runs under one of three postures: `shared`, `isolated` or
`offline`. Set one with `--network`, with `BRIG_NETWORK`, or with a
profile's own `network:` field. A flag beats the setting, and the setting
beats the profile. Leave all three unset and you get `shared`.

```bash
brig run claude --network isolated
```

| posture | what it permits |
| --- | --- |
| `shared` | one network for every sandbox on the host. The default |
| `isolated` | a network of this sandbox's own |
| `offline` | no route out. The agent runs, the guest home is mounted, nothing leaves |

`shared` is a shared network in name, not in reach. Sandboxes on it cannot
reach each other on either macOS backend, but they can reach each other on
Linux. See the per-backend table in
[security.md](security.md#things-brig-does-not-claim) for how that was
measured. `brig info` prints the posture as one of these three lines:

```
NETWORK      shared (one network for every sandbox on this host)
NETWORK      isolated (a network of this sandbox's own)
NETWORK      offline (no egress)
```

`isolated` needs the `hvi` backend on macOS. `vz` and `qemu` take their
network from vmnet, which Brig does not own, so Brig refuses `--network
isolated` there. On Linux, nerdctl creates a network per sandbox for
`isolated`, so the posture works on any Linux host.

Binding an egress policy to a sandbox forces the `isolated` posture, whether
or not `--network` asked for it. See
[Where a policy is enforced, and where it is not](#where-a-policy-is-enforced-and-where-it-is-not).

An unrecognized value refuses the run rather than picking a posture nobody
asked for, and names where the value came from:

```console
$ BRIG_NETWORK=bogus brig info claude-code
brig: BRIG_NETWORK "bogus" is not a posture: use shared, isolated or offline
```

## Writing a policy

A policy is a named YAML (or JSON) document declaring what an agent can
reach outbound. It sets a default of `allow` or `deny`, plus `host` or
`cidr` exceptions on either side. `brig policy create` writes a starter and
opens it in your editor, the same way `brig agent edit` does:

```bash
brig policy create locked-down   # writes ~/.config/brig/policies/locked-down.yaml
brig policy edit locked-down     # change the rules
```

See
[Where a policy is enforced, and where it is not](#where-a-policy-is-enforced-and-where-it-is-not)
for what a bound policy actually gets you.

## Where policies live

One file per policy in `$XDG_CONFIG_HOME/brig/policies`, default
`~/.config/brig/policies`, flat: `~/.config/brig/policies/locked-down.yaml`.
`BRIG_POLICY_DIR` overrides the location outright, taken as given. Unlike
`$XDG_CONFIG_HOME`, an explicit override is not second-guessed for
absoluteness. This follows the
[XDG Base Directory Specification, version 0.8](https://specifications.freedesktop.org/basedir/latest/):
an empty or relative `$XDG_CONFIG_HOME` counts as unset.

The directory starts empty, and Brig never writes there unless you ask it
to. `brig policy create` and `brig policy edit` are the only commands that
write to it.

`name:` inside the file wins over the filename, the same rule a profile
already follows. A file need not be named after the policy it declares,
though `create` always names them the same way. A directory can hold any
number of policies. One file that fails to parse does not stop the others
loading: `brig policy ls` reports it on stderr, and lists everything that
did load. Two files declaring the same name is a mistake with no winner
worth having, and is reported the same way.

## The document

A complete example:

```yaml
apiVersion: brig.sh/v1alpha1
name: locked-down
desc: only Anthropic's API and one internal range
egress:
  default: deny
  allow:
    - host: api.anthropic.com
    - cidr: 10.0.0.0/8
```

| field | required | what it is |
| --- | --- | --- |
| `apiVersion` | yes | Pins the document shape. `brig.sh/v1alpha1` is the only value this build knows. Anything else is refused rather than guessed at |
| `name` | yes | The policy's identifier. Wins over the filename, and follows the same character rule as a profile name. See [Naming a policy](#naming-a-policy) |
| `desc` | no | One line, shown by `brig policy ls` |
| `egress.default` | yes | `allow` or `deny`, applied to any traffic neither list below names |
| `egress.allow` | no | Exceptions to a `deny` default |
| `egress.deny` | no | A host or range to refuse regardless. At the gateway that enforces the rules, `deny` takes priority over `allow` and over `default`. Brig applies no priority of its own: it passes every rule through as a flag. See [Where a policy is enforced](#where-a-policy-is-enforced-and-where-it-is-not) |

Each entry in `allow` or `deny` names exactly one of `host:` or `cidr:`. Both,
or neither, is refused. `host:` is a domain, or a glob such as
`"*.githubusercontent.com"`. `cidr:` is a network range such as
`10.0.0.0/8`, checked with Go's own `net.ParseCIDR`. A typo like
`10.0.0/8` (an octet short) is refused rather than accepted and silently
doing nothing at the gateway that enforces it.

`host:` is not held to a pinned glob grammar here. Which wildcard forms an
enforcer honors is that enforcer's business, and this document format is
deliberately independent of it. A host is refused only for what is
unambiguously wrong however it ends up read: whitespace or a control
character. The gateway that enforces it today matches the glob against the
name the guest asks its resolver for.

Parsing is strict throughout. A field this format does not recognize, such
as `engine:`, `mode:`, or a plain typo like `dsc:`, fails to parse rather
than being silently dropped. The format carries no field naming how a rule
gets applied. That is a deliberate limit, and it stays that way as this
feature grows.

## Naming a policy

The same character rule a profile name already follows: lowercase letters,
digits, dot, dash and underscore, starting with a letter or digit. It is
checked before a path is built from it, so a bad name never gets as far as
touching disk.

One rule is particular to policies. A bare word like `no`, `true` or `123`
is inside that character set. But YAML reads an *unquoted* one of those as
a boolean or a number rather than as the string you typed. A policy named
`no` is actually named `false`, unreachable by the name you gave it.
`brig policy create` checks a name by writing it the way the starter
template writes it, then reading the result back. It refuses one that does
not come back as itself:

```console
$ brig policy create no
brig: name "no" reads as false when written unquoted in YAML, not as itself; pick a different name
```

## The verbs

| verb | what it does |
| --- | --- |
| `brig policy ls` | every policy that parses, by name and description, and, for one bound to anything, what binds it |
| `brig policy create <name>` | write a starter document, then open it: `$VISUAL`, then `$EDITOR`, then `vi` |
| `brig policy edit <name> [--force]` | open an existing one, and only replace it if the save still parses and validates. Refuses a rename that orphans anything bound to it, inline or attached, unless `--force` |
| `brig policy show <name> [--json]` | print the parsed document |
| `brig policy rm <name> [--force]` | delete it. Refuses one that is bound to anything, inline or attached, unless `--force` |
| `brig policy attach <policy> <profile> [-n NAME]` | bind it to every run of a profile, or, with `-n`, to one session by name instead |
| `brig policy detach <policy> <profile> [-n NAME]` | reverse an attach |
| `brig policy check <profile> [-n NAME]` | list what is effectively bound to a run of the profile (or `-n` session), and report whether Brig can enforce anything against it at all |

Complete command lines for all eight:

```bash
brig policy ls
brig policy create locked-down
brig policy edit locked-down
brig policy show locked-down --json
brig policy attach locked-down claude-code
brig policy detach locked-down claude-code
brig policy check claude-code
brig policy rm locked-down --force
```

`create` refuses to overwrite a file that is already at the target path,
unless you pass `--force`. It refuses a name already taken by some *other*
file regardless of `--force`. Forcing leaves two files declaring the same
name, which is the thing this check exists to prevent.

`attach` and `detach` write to `attachments.yaml` in the same directory, not
to the policy or the profile. `attach` refuses, and writes nothing, in
three cases. Either name does not exist. The profile is `kind: shell` or
`kind: gui`, which has no agent to hook an egress rule into. Or the profile
already declares the policy inline in its own `policy:` list. Attaching it
again adds an entry `detach` can never remove:

```console
$ brig policy attach locked-down claude-code
attached locked-down to claude-code
note: enforced on the hvi backend, which gives the sandbox a network of its own; a run on any other backend is refused rather than left unenforced
$ brig policy attach locked-down claude-code -n refactor
attached locked-down to claude-code -n refactor
note: enforced on the hvi backend, which gives the sandbox a network of its own; a run on any other backend is refused rather than left unenforced
$ brig policy attach locked-down ubuntu
brig: cannot attach locked-down to ubuntu: ubuntu is kind: shell, which has no agent to hook an egress rule into. Nothing was written
```

Both `attach` and `check` print that last line. "attached" and a
`check` that prints a policy name can both read as a rule already in force.
See [Where a policy is enforced, and where it is not](#where-a-policy-is-enforced-and-where-it-is-not).
It goes to stderr, where this CLI puts every advisory, so stdout stays the
command's answer. Both commands print the same constant from
`internal/policy`, so the two cannot drift into saying different things.

`detach` reverses `attach`:

```console
$ brig policy detach locked-down claude-code -n refactor
detached locked-down from claude-code -n refactor
```

`detach` refuses a policy the profile declares inline, the same way: it was
never `attach`'s to add, so it is not `detach`'s to remove. Edit the
profile's `policy:` list directly instead. A `-n` detach is unaffected.
Inline binds every run, `-n` narrows to one session, and the two do not
name the same binding.

`check` resolves the same union `attach`/`detach` write to (inline,
profile-level, session-level) for one profile, or, with `-n`, one of its
sessions. It lists what applies, and runs the same `CheckCoverage` refusal
`attach` does:

```console
$ brig policy check claude-code
locked-down
note: enforced on the hvi backend, which gives the sandbox a network of its own; a run on any other backend is refused rather than left unenforced
$ brig policy check ubuntu
no policy applies to ubuntu
brig: cannot enforce any policy on ubuntu: ubuntu is kind: shell, which has no agent to hook an egress rule into
```

"Whether Brig can enforce it" means exactly two structural checks. The
first is whether the profile is `kind: shell` or `kind: gui`. Neither of
those can ever enforce a policy. The second is whether every bound name
still resolves to a policy that loaded.

`check` does not resolve the current runtime, the current hypervisor, or
the runtime's version. It cannot tell you whether the host you are on
right now will boot the run or refuse it. See
[Where a policy is enforced, and where it is not](#where-a-policy-is-enforced-and-where-it-is-not)
for the checks that answer that.

`--force` on `rm`, or on a rename, can leave a binding pointing at a name
nothing loads under any more:

```console
$ brig policy rm locked-down --force
removed /home/you/.config/brig/policies/locked-down.yaml
$ brig policy check claude-code
locked-down (not loaded)
brig: claude-code is bound to locked-down, which no policy loads under -- nothing can enforce what did not load
```

`brig policy ls` prints what binds a policy right under it, when anything
does. That is an inline `policy:` entry, a profile-level attach, or
`<profile> -n <session>` for a session-level one:

```console
$ brig policy ls
locked-down     only Anthropic's API and one internal range
                bound to: claude-code, claude-code -n refactor
```

"not loaded" rather than "no such policy", because there are two ways to
get there, and Brig cannot always tell them apart. Either nothing declares
that name, or the file that declares it did not parse. In that second case
the file and its parse error are named separately on stderr.

`rm` refuses a policy that is bound to anything (an inline `policy:` entry,
a profile-level attach, or a session-level one) unless you pass `--force`.
The file is gone either way, but whatever named it is still pointing at
nothing:

```console
$ brig policy rm locked-down
brig: locked-down is bound to claude-code. Detach it first, or pass --force to remove it anyway
$ brig policy rm locked-down --force
removed /home/you/.config/brig/policies/locked-down.yaml
```

The instruction fits what is actually bound. It says "detach it" for an
attach, "edit the profile's `policy:` list" for an inline entry, or both
when a policy is bound both ways. `detach` explicitly refuses to touch an
inline entry, so telling you to detach one is a dead end.

`edit` never touches the real file until the new content is known to be
good. It opens a scratch copy, and only replaces the original if that copy
still parses and validates. The replace goes through a temp file and a
rename in the same directory. A crash or a full disk mid-write cannot
leave the real file half written:

```console
$ brig policy edit locked-down
brig: not saved, /home/you/.config/brig/policies/locked-down.yaml is unchanged: cidr "10.0.0/8" is not a valid CIDR: invalid CIDR address: 10.0.0/8
your edit is still at /tmp/brig-policy-edit-2427992151.yaml
```

Renaming it (changing `name:` to something else) is refused the same way if
the old name is bound to anything. The binding then points at a name
nothing declares:

```console
$ brig policy edit locked-down
brig: not saved, /home/you/.config/brig/policies/locked-down.yaml is unchanged: renaming locked-down to totally-new would leave claude-code pointing at a name nothing declares. Detach it first, or pass --force to rename it anyway
your edit is still at /tmp/brig-policy-edit-2427992151.yaml
```

A save that keeps the same name never triggers this check: the file a
binding points at is still right there either way.

## Binding one session, not every run

`policy attach` and `policy detach` take `-n NAME` to bind or unbind one
session instead of every run of a profile. `policy check` takes it to ask
about that one session instead of the profile as a whole.

`-n NAME` must already be the slug form of the name: lowercase letters,
digits, dot, dash and underscore. `attach -n Refactor` is refused outright,
naming the slug it must become. This is stricter than the retired
<!-- retired-ok -->`brig run --name`, which sanitizes a name and reports the
directory it landed on.

That difference matters when a session started with `--name` picks up its
policy. The `<agent>@<label>` ref form is unaffected. `ParseRef` refuses
any label that is not already slug-clean, so `claude@refactor` can only
ever reach a session whose stored name is `refactor`. That matches what
`attach -n refactor` requires.

A session opened with `brig run claude --name Refactor` <!-- retired-ok --> is
a different case. `brig run` accepts and sanitizes `Refactor` to the slug `refactor`
for the sandbox itself. But the policy binding is looked up under the name
exactly as typed, `Refactor`, not the slug. Because `attach -n` never
accepts anything but a slug, no attachment is ever filed under `Refactor`.
That session's policy lookup never finds one, even after `brig policy
attach locked-down claude-code -n refactor`.

Until this is fixed, scope any per-session policy claim to the `@` form.
Use `claude@refactor` when you need a policy bound to one session, and
attach it with `brig policy attach locked-down claude-code -n refactor`.
Do not rely on `-n` matching a session opened with `--name` unless the name
you passed to `--name` was already lowercase, slug-clean text.

## A worked example

Starting from nothing:

```console
$ brig policy ls
no policies yet; your own live in /home/you/.config/brig/policies
brig policy create <name> writes a starter one
```

Create one. The starter opens in your editor. Here it has already been
filled in:

```console
$ brig policy create locked-down
/home/you/.config/brig/policies/locked-down.yaml created
$ brig policy ls
locked-down     only Anthropic's API and one internal range
```

Show it, as YAML or as JSON:

```console
$ brig policy show locked-down
apiVersion: brig.sh/v1alpha1
desc: only Anthropic's API and one internal range
egress:
  allow:
  - host: api.anthropic.com
  - cidr: 10.0.0.0/8
  default: deny
name: locked-down
$ brig policy show locked-down --json
{
  "apiVersion": "brig.sh/v1alpha1",
  "name": "locked-down",
  "desc": "only Anthropic's API and one internal range",
  "egress": {
    "default": "deny",
    "allow": [
      { "host": "api.anthropic.com" },
      { "cidr": "10.0.0.0/8" }
    ]
  }
}
```

`show` prints the parsed document back out, not the file verbatim, which is
why the field order differs from what you typed. YAML's own marshalling
sorts keys, the same way `brig agent show --json` does.

Edit it, and remove it:

```console
$ brig policy edit locked-down
/home/you/.config/brig/policies/locked-down.yaml updated
$ brig policy rm locked-down
removed /home/you/.config/brig/policies/locked-down.yaml
```

## Errors you are likely to meet

| what Brig says | what happened |
| --- | --- |
| ``unknown policy "x". `brig policy ls` lists them`` | `show`, `edit` or `rm` on a name that is not there |
| `name "x" may use only lowercase letters, digits, dot, dash and underscore, and must start with a letter or digit` | see [Naming a policy](#naming-a-policy) |
| `name "x" reads as false when written unquoted in YAML, not as itself; pick a different name` | the name is a bare YAML boolean, null or number word. See [Naming a policy](#naming-a-policy) |
| ``<path> already exists. Edit it directly with `brig policy edit x`, or pass --force to replace it with a fresh starter`` | `create` on a name whose file is already there |
| ``policy "x" already exists, declared in <path>. Edit it directly with `brig policy edit x`, or remove that file first`` | `create` on a name a *different* file already declares. `--force` does not help here |
| `a rule needs host: or cidr:` | a rule in `allow:`/`deny:` named neither |
| `a rule takes host: or cidr:, not both …` | a rule named both |
| `cidr "x" is not a valid CIDR: …` | a typo in a `cidr:` value, such as a missing octet |
| `host "x" contains whitespace or a control character` | a `host:` value that cannot be a domain or glob under any grammar |
| `apiVersion is required, and must be "brig.sh/v1alpha1"` | a document with no `apiVersion:`, or the wrong one |
| `not saved, <path> is unchanged: …` | `edit`'s save did not parse or validate, or renamed a name that is bound to something, without `--force`. The real file is untouched. The error names where your edit still is |
| ``unknown profile "x". `brig agent ls` lists them`` | `attach`, `detach` or `check` naming a profile that is not there |
| `cannot attach x to y: y is kind: shell, which has no agent to hook an egress rule into. Nothing was written` | `attach` to a `kind: shell` or `kind: gui` profile |
| `cannot enforce any policy on x: x is kind: shell, which has no agent to hook an egress rule into` | `check` on a `kind: shell` or `kind: gui` profile |
| `x is bound to y, which no policy loads under -- nothing can enforce what did not load` | `check` on a profile bound to a name nothing loads under: either `--force` on `rm` or a rename left no policy behind it, or the file that declares it did not parse (named separately on stderr) |
| `x is already declared inline in y's policy: list, which binds every run already. Nothing was written` | `attach` naming a policy the profile's own `policy:` list already declares |
| `x is declared inline in y's policy: list, not attached; edit the profile directly to remove it` | `detach` naming a policy the profile's own `policy:` list declares, without `-n` |
| `x is bound to y. Detach it first, or pass --force to remove it anyway` | `rm` on a policy attached to a profile or a session (a policy declared only inline says "edit the profile's policy: list" instead) |
| ``a policy applies to this sandbox, but <bin> cannot enforce one (its network-gateway has no --egress-default). Upgrade the runtime, or detach the policy`` | the runtime is older than the hull that added the `--egress-*` gateway flags |

## The default is no policy at all

Read from the source rather than assumed: the default egress stance is
allow everything, with no filtering applied at all. It is not a deny-all
default, and it is not an empty allow list either. A sandbox nobody
attached a policy to has unrestricted egress, exactly as it did before any
of this existed. No profile Brig ships binds a policy, and `brig run
<agent>` on a fresh install filters nothing. No gateway is given a rule
until a policy is attached to that profile or that session by hand.

That is deliberate, and it is a test rather than an intention
(`TestNoShippedProfileBindsAPolicy`, `TestASandboxWithNoPolicyIsUnfilteredAndShared`).
An agent that cannot reach its own API is not a safer agent. It is a
broken one, and a default that broke every sandbox on upgrade costs
everyone, to benefit the few runs that want a rule.

Note the shape of the two defaults, which are easy to confuse. Attaching no
policy means no filtering. Attaching a policy whose `default:` is `deny`
means the opposite: everything is refused except what its `allow` list
names. An empty `allow` list under it is a sandbox with no way out. The
first is what you get. The second is what you ask for.

## Where a policy is enforced, and where it is not

Exactly one backend can enforce an egress policy: hull's `hvi` backend, at
the user-mode network gateway Brig gives that sandbox. `vz` and `qemu` take
their network from vmnet, and every Linux runtime takes its network from
the container network. Neither of those is something Brig filters.

On the `hvi` backend, a boot reads every policy bound to the run and puts
the rules on the network gateway it gives that sandbox. That gateway is the
sandbox's only way out, so the rules are the sandbox's only way out.
Measured in a real guest in
[docs/manual-tests/egress-policy.md](manual-tests/egress-policy.md): an
allowed name reaches, a denied name does not resolve, and an address
dialled directly does not connect.

On every backend that cannot enforce a policy, Brig refuses the boot rather
than running unenforced. `vz`, `qemu` and every Linux runtime refuse a
filtered run outright, naming the backend that does enforce. Refusing it
beats booting a sandbox that reports a policy and filters nothing. There is
one exception: a policy-carrying run whose posture is `offline` is not
refused on any backend. A sandbox with no route out satisfies every rule
set, regardless of who is watching.

That refusal is checked before anything starts, and again on the path that
finds the sandbox already running. A policy cannot be waved through by the
accident of the sandbox being up already. Every runtime Brig ships refuses
a policy it cannot enforce. This is a guarantee about the runtimes Brig
ships today, not a property of the interface. A runtime that never answers
the question is never asked, and stays unrefused.

The runtime has to be new enough, too. The gateway's `--egress-*` flags
arrived after hull 0.1.0-rc21. Brig probes the binary itself, `<bin>
network-gateway --help`, rather than checking a version number. The exact
release does not matter: a hull newer than 0.1.0-rc21 works. An older one
is refused by name once a policy applies to the run:

```console
$ brig policy attach locked-down claude-code
attached locked-down to claude-code
note: enforced on the hvi backend, which gives the sandbox a network of its own; a run on any other backend is refused rather than left unenforced
$ brig run claude
brig: a policy applies to this sandbox, but hull cannot enforce one (its network-gateway has no --egress-default). Upgrade the runtime, or detach the policy -- brig will not boot a sandbox that reports a policy nothing enforces
```

This probe has a gap, worth stating plainly rather than hiding. If the
probe command itself fails to run, Brig draws no conclusion from that
failure, and the boot proceeds anyway. The reasoning is that a runtime too
broken to print its own help produces a clearer error later in the boot.
But the probe cannot tell a broken binary from one that exits non-zero on
`--help` for some other reason. In that one case, the capability check
fails open. A policy can reach a gateway that does not take the flags,
with no refusal from this check first.

Binding a policy has these properties:

- **The rules are fixed when the sandbox boots.** They go on the gateway's
  command line and it reads them once. Editing a policy changes what the
  next boot enforces, and no environment variable overrides a running
  gateway's rules. A sandbox that is already up when the rules change does
  not keep running under the old ones. Brig detects the mismatch, stops,
  removes and reboots the sandbox, and warns that any other session on it
  will be disconnected.
- **A policy takes the network posture with it.** Rules belong to a
  gateway and cover every member of its network. A sandbox answering to
  rules of its own gets a network of its own: the run is `isolated`,
  whether or not it asked to be. The `NETWORK` row of the execution
  envelope says so. That only ever narrows what was asked for.
- **Several policies at once are unioned.** A rule in any of them is a
  rule of the run's. The default is the strictest any of them names: one
  `deny` makes the run deny-by-default. A host the second policy allows is
  reachable even when the first policy alone denies it. Attaching is
  granting. At the gateway that enforces the rules, `deny` still takes
  priority over `allow` across the whole set. See the precedence note
  below.

### What this does not do yet

`attach` and `check` refuse what Brig knows it cannot enforce at all (a
`kind: shell`/`kind: gui` profile), and a name bound to nothing. Neither
inspects the rules a policy contains. Neither can tell you that an `allow`
glob matches nothing you meant. They can only tell you that the document
parses.

Deny-over-allow-over-default ordering, and the host-rule behavior below,
are the enforcing gateway's documented behavior, not something Brig
verifies itself. Brig's own merge applies no priority. It concatenates the
`allow` and `deny` lists from every bound policy. Each rule reaches the
gateway as an `--egress-allow` or `--egress-deny` flag on its command
line.

The one measurement in this repository,
[docs/manual-tests/egress-policy.md](manual-tests/egress-policy.md), covers
a `default: deny` policy with one `host` allow. The allowed name reaches, a
denied name fails to resolve, and an address dialled directly fails to
connect. It does not exercise a `deny` rule overriding an `allow` rule, and
it does not exercise a `default: allow` policy. Treat deny-over-allow
ordering as hull's documented gateway behavior, evidenced historically by
that one measurement, and not as something Brig's own tests confirm.

A `host` rule is enforced through the gateway's own resolver, so what it
covers depends on the default. Under `default: deny`, the resolver answers
only names an `allow` glob covers, and the guest reaches nothing it did not
resolve there. That is also why traffic sent straight to an address, DNS
over HTTPS and DNS over TLS do not get out. Under `default: allow`, a
`host` deny is best effort, because traffic sent straight to an address
never asks for a name. A `cidr` rule is matched on the address either way.
