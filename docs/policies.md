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
it to a profile or a session.** Anything actually enforcing it -- reading a
policy's rules and acting on them -- is not built yet; see
[What this does not do yet](#what-this-does-not-do-yet).

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
doing nothing once something enforces it.

`host:` is not held to a pinned glob grammar yet -- which wildcard forms an
eventual enforcer honours is enforcer-specific, and this document format is
deliberately independent of that question (see the intro above). A host is
refused only for what is unambiguously wrong however it ends up read:
whitespace or a control character.

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
$ brig policy attach no-net claude-code -n work
attached no-net to claude-code -n work
$ brig policy attach no-net ubuntu
brig: cannot attach no-net to ubuntu: ubuntu is kind: shell, which has no agent to hook an egress rule into. Nothing was written
```

`detach` refuses a policy the profile declares inline, the same way: it was
never `attach`'s to add, so it is not `detach`'s to remove. Edit the
profile's `policy:` list directly instead. A `-n` detach is unaffected --
inline binds every run, `-n` narrows to one session, and the two do not
name the same binding.

`brig policy ls` prints what binds a policy right under it, when anything
does -- an inline `policy:` entry, a profile-level attach, or
`<profile> -n <session>` for a session-level one:

```console
$ brig policy ls
no-net          only Anthropic's API and one internal range
                bound to: claude-code, claude-code -n work
```

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
| ``unknown profile "x". `brig profiles` lists them`` | `attach` or `detach` naming a profile that is not there |
| `cannot attach x to y: y is kind: shell, which has no agent to hook an egress rule into. Nothing was written` | `attach` to a `kind: shell` or `kind: gui` profile |
| `x is already declared inline in y's policy: list, which binds every run already. Nothing was written` | `attach` naming a policy the profile's own `policy:` list already declares |
| `x is declared inline in y's policy: list, not attached; edit the profile directly to remove it` | `detach` naming a policy the profile's own `policy:` list declares, without `-n` |
| `x is bound to y. Detach it first, or pass --force to remove it anyway` | `rm` on a policy attached to a profile or a session (a policy declared only inline says "edit the profile's policy: list" instead) |

## What this does not do yet

This release ships the document format, the commands above, and a record of
what is bound to what -- nothing that reads a policy's rules at boot or
runtime and acts on them. `attach` only refuses a binding brig already
knows it cannot enforce (a `kind: shell`/`kind: gui` profile); it does not
check anything beyond that, `brig policy check` does not exist, and a boot
does not yet refuse to start over an unenforceable rule. Nothing today
makes an agent's outbound traffic actually respect a policy you write.

[docs/security.md](security.md#things-brig-does-not-claim) states this
directly: brig does not sandbox the agent from the network, and outbound
traffic from the guest is whatever the runtime allows.
