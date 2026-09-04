# Writing an egress policy

A policy is a named YAML (or JSON) document declaring what an agent may
reach outbound: a default of `allow` or `deny`, plus `host` or `cidr`
exceptions on either side. `brig policy create` writes a starter and opens
it in your editor, the same way `brig profile edit` does:

```bash
brig policy create no-net   # writes ~/.config/brig/policies/no-net.yaml
brig policy edit no-net     # change the rules
```

**This page is about the document, the commands that manage it, and binding
it to a profile or a session.** A bound policy is enforced on the `hvi`
backend, at the network gateway brig gives that sandbox, and a run on a
backend that cannot enforce one is refused rather than left unconstrained; see
[Where a policy is enforced](#where-a-policy-is-enforced-and-where-it-is-not).

## Where policies live

One file per policy in `$XDG_CONFIG_HOME/brig/policies`, default
`~/.config/brig/policies`, flat: `~/.config/brig/policies/no-net.yaml`.
`BRIG_POLICY_DIR` overrides the location outright, taken as given -- unlike
`$XDG_CONFIG_HOME`, an explicit override is not second-guessed for
absoluteness. This follows the
[XDG Base Directory Specification, version 0.8](https://specifications.freedesktop.org/basedir/latest/):
an empty or relative `$XDG_CONFIG_HOME` counts as unset.

The directory starts empty, and brig never writes there unless you ask it
to. `brig policy create` and `brig policy edit` are the only commands that
write to it.

`name:` inside the file wins over the filename, the same rule a profile
already follows -- a file need not be named after the policy it declares,
though `create` always names them the same way. A directory can hold any
number of policies, and one file that fails to parse does not stop the
others from loading: `brig policies` reports it on stderr and lists
everything that did load. Two files declaring the same name is a mistake
with no winner worth having, and is reported the same way.

## The document

A complete example:

```yaml
apiVersion: brig.sh/v1alpha1
name: no-net
desc: only Anthropic's API and one internal range
egress:
  default: deny
  allow:
    - host: api.anthropic.com
    - cidr: 10.0.0.0/8
```

| field | required | what it is |
| --- | --- | --- |
| `apiVersion` | yes | Pins the document shape. `brig.sh/v1alpha1` is the only value this build knows; anything else is refused rather than guessed at |
| `name` | yes | The policy's identifier. Wins over the filename, and follows the same character rule as a profile name -- see [Naming a policy](#naming-a-policy) |
| `desc` | no | One line, shown by `brig policies` |
| `egress.default` | yes | `allow` or `deny`, applied to any traffic neither list below names |
| `egress.allow` | no | Exceptions to a `deny` default |
| `egress.deny` | no | Always wins over `allow` and over `default`: a host named here is refused regardless, and no other settings source restores it |

Each entry in `allow` or `deny` names exactly one of `host:` or `cidr:` --
both, or neither, is refused. `host:` is a domain, or a glob such as
`"*.githubusercontent.com"`; `cidr:` is a network range such as
`10.0.0.0/8`, checked with Go's own `net.ParseCIDR`, so a typo like
`10.0.0/8` (an octet short) is refused rather than accepted and silently
doing nothing at the gateway that enforces it.

`host:` is not held to a pinned glob grammar here -- which wildcard forms an
enforcer honours is that enforcer's business, and this document format is
deliberately independent of it (see the intro above). A host is refused only
for what is unambiguously wrong however it ends up read: whitespace or a
control character. The gateway that enforces it today matches the glob against
the name the guest asks its resolver for.

Parsing is strict throughout: a field this format does not recognise --
`engine:`, `mode:`, a plain typo like `dsc:` -- fails to parse rather than
being silently dropped. The format carries no field naming how a rule gets
applied; that is a deliberate limit, not an oversight, and stays that way as
this feature grows.

## Naming a policy

The same character rule a profile name already follows: lowercase letters,
digits, dot, dash and underscore, starting with a letter or digit. It is
checked before a path is built from it, so a bad name never gets as far as
touching disk.

One rule is particular to policies. A bare word like `no`, `true` or `123`
is inside that character set, but YAML reads an *unquoted* one of those as a
boolean or a number rather than as the string you typed -- so a policy named
`no` would actually be named `false`, unreachable by the name you gave it.
`brig policy create` checks a name by writing it the way the starter
template would and reading it back, and refuses one that does not come back
as itself:

```console
$ brig policy create no
brig: name "no" reads as false when written unquoted in YAML, not as itself; pick a different name
```

## The verbs

| verb | what it does |
| --- | --- |
| `brig policies` | every policy that parses, by name and description, and -- for one bound to anything -- what binds it |
| `brig policy ls` | same (parity with `brig profiles` / `brig profile ls`) |
| `brig policy create <name>` | write a starter document, then open it: `$VISUAL`, then `$EDITOR`, then `vi` |
| `brig policy edit <name> [--force]` | open an existing one, and only replace it if the save still parses and validates. Refuses a rename that would orphan anything bound to it -- inline or attached -- unless `--force` |
| `brig policy show <name> [--json]` | print the parsed document |
| `brig policy rm <name> [--force]` | delete it. Refuses one that is bound to anything -- inline, or attached -- unless `--force` |
| `brig policy attach <policy> <profile> [-n NAME]` | bind it to every run of a profile, or -- with `-n` -- to one session by name instead |
| `brig policy detach <policy> <profile> [-n NAME]` | reverse an attach |
| `brig policy check <profile> [-n NAME]` | list what is effectively bound to a run of the profile (or `-n` session), and whether brig can enforce anything against it at all |

`create` refuses to overwrite a file that is already at the target path,
unless you pass `--force`. It refuses a name already taken by some *other*
file regardless of `--force` -- forcing would only leave two files
declaring the same name, which is the thing this check exists to prevent.

`attach` and `detach` write to `attachments.yaml` in the same directory,
not to the policy or the profile. `attach` refuses, and writes nothing, if
either name does not exist, if the profile is `kind: shell` or `kind: gui`
(no agent to hook an egress rule into), or if the profile already declares
the policy inline in its own `policy:` list -- attaching it again would only
add an entry `detach` could never remove:

```console
$ brig policy attach no-net claude-code
attached no-net to claude-code
note: enforced on the hvi backend, which gives the sandbox a network of its own; a run on any other backend is refused rather than left unenforced
$ brig policy attach no-net claude-code -n work
attached no-net to claude-code -n work
note: enforced on the hvi backend, which gives the sandbox a network of its own; a run on any other backend is refused rather than left unenforced
$ brig policy attach no-net ubuntu
brig: cannot attach no-net to ubuntu: ubuntu is kind: shell, which has no agent to hook an egress rule into. Nothing was written
```

Both `attach` and `check` say that last line, because "attached" and a
`check` that prints a policy name both read as a rule that is in force
everywhere, and where it is in force depends on the backend the run lands on
-- see
[Where a policy is enforced](#where-a-policy-is-enforced-and-where-it-is-not).
It goes to stderr, where this CLI puts every advisory, so stdout stays the
command's answer. Both commands print the same constant from `internal/policy`, so
the two cannot drift into saying different things.

`detach` refuses a policy the profile declares inline, the same way: it was
never `attach`'s to add, so it is not `detach`'s to remove. Edit the
profile's `policy:` list directly instead. A `-n` detach is unaffected --
inline binds every run, `-n` narrows to one session, and the two do not
name the same binding.

`check` resolves the same union `attach`/`detach` write to -- inline,
profile-level, session-level -- for one profile (or, with `-n`, one of its
sessions), lists what applies, and runs the same `CheckCoverage` refusal
`attach` does. It does not check anything about the rules those policies
contain (see [Where a policy is enforced](#where-a-policy-is-enforced-and-where-it-is-not)):
the only two things it can fail over are a `kind: shell`/`kind: gui`
profile, which nothing could ever enforce against no matter what is
bound, and a name bound to nothing -- `--force` on `rm`, or on this
edit's rename check, can leave one behind, and `check` refuses to call
that enforceable:

```console
$ brig policy check claude-code
no-net
note: enforced on the hvi backend, which gives the sandbox a network of its own; a run on any other backend is refused rather than left unenforced
$ brig policy check ubuntu
no policy applies to ubuntu
brig: cannot enforce any policy on ubuntu: ubuntu is kind: shell, which has no agent to hook an egress rule into
$ brig policy rm no-net --force
removed /home/you/.config/brig/policies/no-net.yaml
$ brig policy check claude-code
no-net (not loaded)
brig: claude-code is bound to no-net, which no policy loads under -- nothing can enforce what did not load
```

`brig policy ls` prints what binds a policy right under it, when anything
does -- an inline `policy:` entry, a profile-level attach, or
`<profile> -n <session>` for a session-level one:

```console
$ brig policy ls
no-net          only Anthropic's API and one internal range
                bound to: claude-code, claude-code -n work
```

`--force` on `rm`, or on a rename, leaves a binding pointing at a name
nothing loads under any more. Both say so at the time, and then nothing
does, so the listing is where that leftover reappears:

```console
$ brig policy rm no-net --force
removed /home/you/.config/brig/policies/no-net.yaml
$ brig policy ls
no-net          (not loaded; bound to claude-code)
```

"not loaded" rather than "no such policy", because there are two ways to
get there and brig cannot always tell them apart: either nothing declares
that name, or the file that declares it did not parse -- and in that
second case the file and its parse error are named separately on stderr.

`rm` refuses a policy that is bound to anything -- an inline `policy:`
entry, a profile-level attach, or a session-level one -- unless you pass
`--force`: the file would be gone, but whatever named it would still be
pointing at nothing. `--force` removes it anyway and leaves that now-
dangling reference in place -- `rm`'s job is the file, not what points
at it:

```console
$ brig policy rm no-net
brig: no-net is bound to claude-code. Detach it first, or pass --force to remove it anyway
$ brig policy rm no-net --force
removed /home/you/.config/brig/policies/no-net.yaml
```

The instruction fits what is actually bound: "detach it" for an attach,
"edit the profile's `policy:` list" for an inline entry, or both when a
policy is bound both ways -- detach explicitly refuses to touch an inline
entry (below), so telling you to detach one would be a dead end.

Unlike `brig policy ls`, which only degrades to a plainer listing if
`attachments.yaml` cannot be read, `rm` without `--force` refuses outright
in that case: it would rather stop than delete something it could not
confirm was safe to.

`edit` never touches the real file until the new content is known to be
good: it opens a scratch copy, and only if that copy still parses and
validates does it replace the original -- via a temp file and a rename in
the same directory, so a crash or a full disk mid-write cannot leave the
real file half written. A save that does not validate leaves the real file
untouched and says where your edit still is, so it is not lost, only not
yet saved:

```console
$ brig policy edit no-net
brig: not saved, /home/you/.config/brig/policies/no-net.yaml is unchanged: cidr "10.0.0/8" is not a valid CIDR: invalid CIDR address: 10.0.0/8
your edit is still at /tmp/brig-policy-edit-2427992151.yaml
```

Renaming it (changing `name:` to something else) is refused the same way
if the old name is bound to anything -- an attach, or a profile's own
inline `policy:` entry -- since the binding would then be pointing at a
name nothing declares:

```console
$ brig policy edit no-net
brig: not saved, /home/you/.config/brig/policies/no-net.yaml is unchanged: renaming no-net to totally-new would leave claude-code pointing at a name nothing declares. Detach it first, or pass --force to rename it anyway
your edit is still at /tmp/brig-policy-edit-2427992151.yaml
```

A save that keeps the same name never triggers this check: the file a
binding points at is still right here either way.

## A worked example

Starting from nothing:

```console
$ brig policies
no policies yet; your own live in /home/you/.config/brig/policies
brig policy create <name> writes a starter one
```

Create one. The starter opens in your editor; here it has already been
filled in:

```console
$ brig policy create no-net
/home/you/.config/brig/policies/no-net.yaml created
$ brig policies
no-net          only Anthropic's API and one internal range
```

Show it, as YAML or as JSON:

```console
$ brig policy show no-net
apiVersion: brig.sh/v1alpha1
desc: only Anthropic's API and one internal range
egress:
  allow:
  - host: api.anthropic.com
  - cidr: 10.0.0.0/8
  default: deny
name: no-net
$ brig policy show no-net --json
{
  "apiVersion": "brig.sh/v1alpha1",
  "name": "no-net",
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
why the field order differs from what you typed -- YAML's own marshalling
sorts keys, the same way `brig profile export --json` does.

Edit it, and remove it:

```console
$ brig policy edit no-net
/home/you/.config/brig/policies/no-net.yaml updated
$ brig policy rm no-net
removed /home/you/.config/brig/policies/no-net.yaml
```

## Errors you are likely to meet

| what brig says | what happened |
| --- | --- |
| ``unknown policy "x". `brig policies` lists them`` | `show`, `edit` or `rm` on a name that is not there |
| `name "x" may use only lowercase letters, digits, dot, dash and underscore, and must start with a letter or digit` | see [Naming a policy](#naming-a-policy) |
| `name "x" reads as false when written unquoted in YAML, not as itself; pick a different name` | the name is a bare YAML boolean, null or number word -- see [Naming a policy](#naming-a-policy) |
| `<path> already exists. Edit it directly with brig policy edit x, or pass --force to replace it with a fresh starter` | `create` on a name whose file is already there |
| `policy "x" already exists, declared in <path>. Edit it directly with brig policy edit x, or remove that file first` | `create` on a name a *different* file already declares. `--force` does not help here |
| `a rule needs host: or cidr:` | a rule in `allow:`/`deny:` named neither |
| `a rule takes host: or cidr:, not both …` | a rule named both |
| `cidr "x" is not a valid CIDR: …` | a typo in a `cidr:` value, such as a missing octet |
| `host "x" contains whitespace or a control character` | a `host:` value that cannot be a domain or glob under any grammar |
| `apiVersion is required, and must be "brig.sh/v1alpha1"` | a document with no `apiVersion:`, or the wrong one |
| `not saved, <path> is unchanged: …` | `edit`'s save did not parse or validate, or renamed a name that is bound to something, without `--force`. The real file is untouched; the error names where your edit still is |
| ``unknown profile "x". `brig profiles` lists them`` | `attach`, `detach` or `check` naming a profile that is not there |
| `cannot attach x to y: y is kind: shell, which has no agent to hook an egress rule into. Nothing was written` | `attach` to a `kind: shell` or `kind: gui` profile |
| `cannot enforce any policy on x: x is kind: shell, which has no agent to hook an egress rule into` | `check` on a `kind: shell` or `kind: gui` profile |
| `x is bound to y, which no policy loads under -- nothing can enforce what did not load` | `check` on a profile bound to a name nothing loads under: either `--force` on `rm` or a rename left no policy behind it, or the file that declares it did not parse (named separately on stderr) |
| `x is already declared inline in y's policy: list, which binds every run already. Nothing was written` | `attach` naming a policy the profile's own `policy:` list already declares |
| `x is declared inline in y's policy: list, not attached; edit the profile directly to remove it` | `detach` naming a policy the profile's own `policy:` list declares, without `-n` |
| `x is bound to y. Detach it first, or pass --force to remove it anyway` | `rm` on a policy attached to a profile or a session (a policy declared only inline says "edit the profile's policy: list" instead) |

## The default is no policy at all

A sandbox nobody attached a policy to has **unrestricted egress**, exactly as
it did before any of this existed. No profile brig ships binds a policy, `brig
run <agent>` on a fresh install filters nothing, and no gateway is given a rule
until a policy is attached to that profile or that session by hand.

That is deliberate, and it is a test rather than an intention
(`TestNoShippedProfileBindsAPolicy`, `TestASandboxWithNoPolicyIsUnfilteredAndShared`).
An agent that cannot reach its own API is not a safer agent, it is a broken
one, and a default that broke every sandbox on upgrade would be paid for by
everyone to benefit the few runs that want a rule.

Note the shape of the two defaults, which are easy to confuse. Attaching no
policy means **no filtering**. Attaching a policy whose `default:` is `deny`
means the opposite -- everything is refused except what its `allow` list names
-- and an empty `allow` list under it is a sandbox with no way out. The first
is what you get; the second is what you ask for.

## Where a policy is enforced, and where it is not

On the `hvi` backend, a boot reads every policy bound to the run and puts the
rules on the network gateway it gives that sandbox. That gateway is the
sandbox's only way out, so the rules are the sandbox's only way out. Measured
in a real guest in
[docs/manual-tests/egress-policy.md](manual-tests/egress-policy.md): an allowed
name reaches, a denied name does not resolve, and an address dialled directly
does not connect.

Everywhere else the boot is **refused** rather than left unenforced. `vz` and
`qemu` take their network from vmnet and the Linux runtimes from the container
network, neither of which brig filters; booting there with a policy attached
would give a sandbox that reports one and enforces nothing, which is worse than
no policy at all, because someone would rely on it. The refusal names the
backend that does enforce.

The runtime has to be new enough, too. The gateway's rule flags arrived after
hull 0.1.0-rc21, so brig asks the binary whether it takes them and says so
plainly if it does not, rather than letting the sandbox come up with no network
and the reason in a log file.

Three properties worth stating outright:

- **The rules are fixed when the sandbox boots.** They go on the gateway's
  command line and it reads them once. Editing a policy changes what the next
  boot enforces, never what a running sandbox is already under, and no
  environment variable overrides it. A policy an agent could ask to have
  relaxed mid-run is not a policy.
- **A policy takes the network posture with it.** Rules belong to a gateway and
  cover every member of its network, so a sandbox answering to rules of its own
  gets a network of its own: the run is `isolated` whether or not it asked to
  be, and the `NETWORK` row of the execution envelope says so. That only ever
  narrows what was asked for.
- **Several policies at once are unioned.** A rule in any of them is a rule of
  the run's, and the default is the strictest any of them names -- one `deny`
  makes the run deny-by-default. Note what the union costs: a host allowed by
  the second policy is reachable even though the first alone would have denied
  it. Attaching is granting. `deny` still beats `allow` across the whole set.

### What this still does not do

`attach` and `check` refuse what brig knows it cannot enforce at all (a
`kind: shell`/`kind: gui` profile) and a name bound to nothing, but neither
inspects the rules a policy contains: they cannot tell you that an `allow` glob
matches nothing you meant, only that the document parses.

A `host` rule is enforced through the gateway's own resolver, so what it covers
depends on the default. Under `default: deny` the resolver answers only names
an `allow` glob covers, and the guest reaches nothing it did not resolve there
-- which is also why traffic sent straight to an address, DNS-over-HTTPS and
DNS-over-TLS do not get out. Under `default: allow` a `host` deny is best
effort, because traffic sent straight to an address never asks for a name. A
`cidr` rule is matched on the address either way.
