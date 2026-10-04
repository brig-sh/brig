# Networking and egress policy

A sandbox runs under one of three network postures. You can bind an
egress policy to a profile, or to one session, on top of that.

An egress policy is enforced on two run paths. On macOS it is hull's `hvi`
backend, which needs macOS 15 or newer and a hull newer than 0.1.0-rc21. On
Linux it is nerdctl, which needs `nft` and `nsenter` on the host. It is
measured on rootless nerdctl. Brig treats nerdctl as rootful when Brig itself
runs as root, and that is not measured yet. Every other runtime, including
docker, refuses to boot a policy it
cannot enforce. The one exception is `--network offline`: it reaches no
network at all, so it satisfies any egress rule and is never refused. See
[Where a policy is enforced, and where it is not](#where-a-policy-is-enforced-and-where-it-is-not).

## Network postures

Every sandbox runs under one of three postures: `shared`, `isolated` or
`offline`. Set one with `--network`, with `BRIG_NETWORK`, or with a
profile's own `network:` field. A flag beats the setting, and the setting
beats the profile. A new sandbox defaults to `isolated` on `hvi` and Linux.
The six built-in `hvi` profiles name that posture explicitly.
`claude-desktop` names `shared` because its GUI requires `vz`, where Brig
cannot give a sandbox its own network. The unpublished `cursor` profile
leaves the choice unset: it gets isolation on Linux and `hvi`, and the
`shared` fallback on `vz` and `qemu`.

```bash
brig run claude                         # a new sandbox gets its own network
brig run claude@shared --network shared # explicitly share one with other sandboxes
```

A sandbox keeps the posture it was started with. A later command that
names no posture, such as `brig sh`, `brig info` or a bare `brig run`,
uses that one, and the profile's `network:` does not move it. The
posture kept is the one asked for, not the `isolated` posture a policy
forces. See the note on policies below. To change the posture, name a
different one with `--network` or `BRIG_NETWORK`. The posture is fixed
when a sandbox boots, so Brig restarts it and says which posture it
leaves and which it goes to:

```console
$ brig run claude --network shared
brig: this sandbox was started with the isolated posture and --network asks for shared
  ↳ rules are fixed when a sandbox boots, so brig restarts it
  ↳ any other session using this sandbox will be disconnected
```

Brig records the posture when it boots a sandbox, and drops the record
with `brig rm`. For an existing session without that record, Brig asks the
runtime how its sandbox was configured and keeps that posture ahead of the
profile's default when it can recover that configuration. Reading the
posture does not write a new record or restart the sandbox. A successful
run that reuses it records the recovered posture for later commands.

`sandbox-*.sock`, including case variants, is reserved for isolated
gateways. Brig rejects a shared `BRIG_GATEWAY_SOCK` override using that
name before starting or replacing a networked guest. For an unrecorded Hull
guest using such a socket, Brig reads the `.spec` beside the socket path in
Hull's saved argv. Only isolated gateways write that file, so a readable,
nonempty spec recovers `isolated`, even under a previous gateway directory.
Other gateway names recover as shared. Today's gateway environment does not
choose which spec is read.

A missing, empty or unreadable spec leaves a `sandbox-*.sock` gateway
`unknown`, and a flagless run is refused. `brig stop` removes the spec, and
writing it at gateway startup is best effort: neither case proves shared
networking. An older isolated guest stopped before posture records were
introduced therefore still needs an explicit network choice.

The spec records gateway configuration, not a VM creation identity. Older
versions allowed shared overrides named `sandbox-*.sock`; if one reuses an
old isolated socket path with a leftover spec, this recovery can wrongly
report isolation. Runtime metadata tied to the VM's creation is needed to
remove that ambiguity.

Changing `BRIG_GATEWAY_DIR`, or the directory of `BRIG_GATEWAY_SOCK` when
no gateway directory is set, also changes where Brig reads `networks.json`
and allocator records. Restore the original settings to find those records,
or choose a network explicitly to recreate the guest if its recovered
configuration cannot be reused.
Choose a non-reserved shared socket name before recreating a shared guest.

Recovering an older sandbox's posture does not reconstruct lost allocator
or gateway records. If a recovered posture does not pass the current
gateway consistency check, a flagless run refuses to replace the guest.
Restore its gateway settings or name `--network` explicitly. The normal
consistency checks still apply to sandboxes with a saved posture record.

If Brig cannot establish an existing sandbox's posture, it refuses a run
that names none rather than applying the new default. With no runtime able
to inspect it, `brig info` reports the posture as unknown. Name the intended
posture explicitly with `--network` or `BRIG_NETWORK` to recreate it; that
disconnects any session using it. A sandbox the runtime confirms is absent
uses the defaults for a new sandbox.

Hull rc29's `inspect` cannot distinguish absence from unreadable metadata,
and its listing omits unreadable records. Brig therefore keeps an indexed
legacy session unknown even when Hull says "instance not found". If you
removed the VM directly with `hull rm`, `brig ls` prunes its stale session
entry and `brig rm <ref>` forgets it; you can also name the intended posture
explicitly. Check runtime access and saved state before using any of these
for an unexplained error.
If all session-index evidence is also lost, Hull cannot distinguish that
case from a new name, and Brig's ordinary discovery uses the new-sandbox
default. Reliable absence detection in that case needs a runtime response
that distinguishes a missing record from an unreadable one.

An older release that boots the sandbox again does not update the record.
On a host where two releases share one sandbox, the record can name a
posture the sandbox no longer has, and `brig info` reports the recorded
one. There is one exception: on `hvi`, when the record says `shared` and
the sandbox is behind an isolated gateway, `brig info` names `isolated`.
`brig stop` and a `brig run` from this release boot it again and write a
new record. `brig rm` drops the record with the sandbox, and with the
session when the sandbox was already removed outside Brig.

| posture | what it permits |
| --- | --- |
| `shared` | one network for every sandbox using this posture on the host. Opt-in, except for the `vz` profiles and retained older sessions |
| `isolated` | a network of this sandbox's own. The default for new `hvi` and Linux sandboxes |
| `offline` | no route out. The agent runs, the guest home is mounted, nothing leaves |

Both `shared` and `isolated` permit internet access. Isolation separates
sandboxes; it does not apply an outbound allow list or promise that host
services are unreachable. An egress policy is a separate choice.

Sandboxes on `shared` reach each other on `hvi` and on Linux. `vz` is not
measured on a current hull. `--network isolated` keeps a sandbox off that
network. See the per-backend table in
[security.md](security.md#things-brig-does-not-claim) for how that was
measured. `brig info` prints the posture as one of these three lines:

```
NETWORK      shared (one network for every sandbox on this host)
NETWORK      isolated (a network of this sandbox's own)
NETWORK      offline (no egress)
```

The row names the posture the running sandbox has. When its next boot
gets a different one, for example after a policy is attached or detached,
the row names that one too:

```
NETWORK      isolated (a network of this sandbox's own); shared from its next boot
```

`isolated` needs the `hvi` backend on macOS. `vz` and `qemu` take their
network from vmnet, which Brig does not own, so Brig refuses `--network
isolated` there. A custom profile with no `network:` field falls back to
`shared` on `vz` or `qemu` only when no flag, environment setting or retained
posture names a network; the `NETWORK` row in `brig info` names that backend
fallback. An explicit `isolated` remains an error. In particular, overriding
one of the built-in `hvi` profiles to `vz` or `qemu` also needs
`--network shared` or `BRIG_NETWORK=shared`, because its profile explicitly
asks for isolation.
On Linux, nerdctl creates a network per sandbox for `isolated`.

On `hvi`, isolation also costs one gateway process per sandbox: about
28.7 MB per gateway in the measurement recorded in
[#369](https://github.com/brig-sh/brig/issues/369), not a fixed resource
guarantee. The isolated address pool has 64 networks; exhaustion refuses
another boot. Remove unused sandboxes with `brig rm <ref>` to free their
networks. Linux uses the runtime's network allocation instead of this pool.

Binding an egress policy to a sandbox forces the `isolated` posture, whether
or not `--network` asked for it. The record keeps the posture that was
asked for, so the sandbox goes back to it once the policy is detached.
Until its next boot, `brig info` names `isolated`, the posture it runs
with. To keep `isolated` after a detach, ask for it with `--network
isolated` while the policy is attached. Brig restarts the sandbox to
record that posture. See
[Where a policy is enforced, and where it is not](#where-a-policy-is-enforced-and-where-it-is-not).

Brig refuses a run with an unrecognized value, and names where the value
came from:

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
for what a bound policy enforces.

## Where policies live

One file per policy in `$XDG_CONFIG_HOME/brig/policies`, default
`~/.config/brig/policies`, flat: `~/.config/brig/policies/locked-down.yaml`.
`BRIG_POLICY_DIR` overrides the location and is used as given, relative or
not. An empty or relative `$XDG_CONFIG_HOME` counts as unset, as the
[XDG Base Directory Specification, version 0.8](https://specifications.freedesktop.org/basedir/latest/)
requires.

The directory starts empty. Brig writes there only when you run `brig
policy create`, `edit`, `rm`, `attach` or `detach`.

`name:` inside the file wins over the filename, as it does for a profile. A
file need not be named after the policy it declares, though `create` always
names them the same. A directory can hold any number of policies. One file
that fails to parse does not stop the others loading: `brig policy ls`
reports it on stderr, and lists everything that did load. Two files that
declare the same name are reported the same way.

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
| `apiVersion` | yes | Pins the document shape. `brig.sh/v1alpha1` is the only value this build knows. Anything else is refused |
| `name` | yes | The policy's identifier. Wins over the filename, and follows the same character rule as a profile name. See [Naming a policy](#naming-a-policy) |
| `desc` | no | One line, shown by `brig policy ls` |
| `egress.default` | yes | `allow` or `deny`, applied to any traffic neither list below names |
| `egress.allow` | no | Exceptions to a `deny` default |
| `egress.deny` | no | A host or range to refuse regardless. At the gateway that enforces the rules, `deny` takes priority over `allow` and over `default`. Brig applies no priority of its own: it passes every rule through as a flag. See [Where a policy is enforced](#where-a-policy-is-enforced-and-where-it-is-not) |

Each entry in `allow` or `deny` names exactly one of `host:` or `cidr:`. Both,
or neither, is refused. `host:` is a domain, or a glob such as
`"*.githubusercontent.com"`. `cidr:` is a network range such as
`10.0.0.0/8`, checked with Go's `net.ParseCIDR`. Brig refuses a typo like
`10.0.0/8` (an octet short), so it cannot reach the gateway as a rule that
matches nothing.

Brig does not hold `host:` to a glob grammar. The enforcer decides which
wildcard forms it honors. Brig refuses a host only for whitespace or a
control character. The gateway that enforces it today matches the glob
against the name the guest asks its resolver for.

Parsing is strict. A field the format does not recognize, such as
`engine:`, `mode:`, or a typo like `dsc:`, fails to parse. The format has no
field that names how a rule is applied.

## Naming a policy

A policy name follows the same character rule as a profile name: lowercase
letters, digits, dot, dash and underscore, starting with a letter or digit.
Brig checks the name before it builds a path from it, so a bad name never
reaches disk.

One rule applies only to policies. A bare word like `no`, `true` or `123`
is inside that character set, but YAML reads it unquoted as a boolean or a
number. A policy named `no` would be named `false`, and you could not reach
it by the name you gave it. `brig policy create` writes the name the way
the starter template writes it, reads the result back, and refuses a name
that does not come back as itself:

```console
$ brig policy create no
brig: name "no" reads as false when written unquoted in YAML, not as itself; pick a different name
```

## The verbs

| verb | what it does |
| --- | --- |
| `brig policy ls` | every policy that parses, by name and description, and, for one bound to anything, what binds it |
| `brig policy create <name> [--force]` | write a starter document, then open it: `$VISUAL`, then `$EDITOR`, then `vi` |
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
file regardless of `--force`, because forcing would leave two files
declaring the same name.

`attach` and `detach` write to `attachments.yaml` in the same directory, not
to the policy or the profile. `attach` refuses, and writes nothing, in
three cases:

- Either name does not exist.
- The profile is `kind: shell` or `kind: gui`, which has no agent to hook an
  egress rule into.
- The profile already declares the policy inline in its own `policy:` list.
  Attaching it again would add an entry `detach` cannot remove.

```console
$ brig policy attach locked-down claude-code
attached locked-down to claude-code
note: enforced on the hvi backend and on Linux with nerdctl, which give the sandbox a network of its own; a run on any other backend is refused rather than left unenforced
$ brig policy attach locked-down claude-code -n refactor
attached locked-down to claude-code -n refactor
note: enforced on the hvi backend and on Linux with nerdctl, which give the sandbox a network of its own; a run on any other backend is refused rather than left unenforced
$ brig policy attach locked-down ubuntu
brig: cannot attach locked-down to ubuntu: ubuntu is kind: shell, which has no agent to hook an egress rule into. Nothing was written
```

Both `attach` and `check` print the `note:` line, because "attached" and a
`check` that prints a policy name can both read as a rule already in force.
See [Where a policy is enforced, and where it is not](#where-a-policy-is-enforced-and-where-it-is-not).
The note goes to stderr, so stdout stays the command's answer.

`detach` reverses `attach`:

```console
$ brig policy detach locked-down claude-code -n refactor
detached locked-down from claude-code -n refactor
```

`detach` refuses a policy the profile declares inline. Edit the profile's
`policy:` list instead. A `-n` detach is unaffected: inline binds every
run, `-n` binds one session, and the two are separate bindings.

`check` resolves the same union `attach`/`detach` write to (inline,
profile-level, session-level) for one profile, or, with `-n`, one of its
sessions. It lists what applies, and runs the same `CheckCoverage` refusal
`attach` does:

```console
$ brig policy check claude-code
locked-down
note: enforced on the hvi backend and on Linux with nerdctl, which give the sandbox a network of its own; a run on any other backend is refused rather than left unenforced
$ brig policy check ubuntu
no policy applies to ubuntu
brig: cannot enforce any policy on ubuntu: ubuntu is kind: shell, which has no agent to hook an egress rule into
```

"Whether Brig can enforce it" means two checks. The first is whether the
profile is `kind: shell` or `kind: gui`, which can never enforce a policy.
The second is whether every bound name still resolves to a policy that
loaded.

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

`check` says "not loaded" because Brig cannot always tell two cases apart:
nothing declares that name, or the file that declares it did not parse. In
the second case the file and its parse error are named separately on
stderr.

`rm` refuses a policy that is bound to anything (an inline `policy:` entry,
a profile-level attach, or a session-level one) unless you pass `--force`.
With `--force` the file is removed, and whatever named it then points at
nothing:

```console
$ brig policy rm locked-down
brig: locked-down is bound to claude-code. Detach it first, or pass --force to remove it anyway
$ brig policy rm locked-down --force
removed /home/you/.config/brig/policies/locked-down.yaml
```

The message names the fix for what is bound: "Detach it" for an attach,
"Edit the profile's policy: list" for an inline entry, or both when a
policy is bound both ways.

`edit` opens a scratch copy, and replaces the original only if that copy
still parses and validates. The replace goes through a temp file and a
rename in the same directory, so a crash or a full disk mid-write cannot
leave the real file half written:

```console
$ brig policy edit locked-down
brig: not saved, /home/you/.config/brig/policies/locked-down.yaml is unchanged: cidr "10.0.0/8" is not a valid CIDR: invalid CIDR address: 10.0.0/8
your edit is still at /tmp/brig-policy-edit-2427992151.yaml
```

Renaming it (changing `name:`) is refused the same way if the old name is
bound to anything, because the binding would then point at a name nothing
declares:

```console
$ brig policy edit locked-down
brig: not saved, /home/you/.config/brig/policies/locked-down.yaml is unchanged: renaming locked-down to totally-new would leave claude-code pointing at a name nothing declares. Detach it first, or pass --force to rename it anyway
your edit is still at /tmp/brig-policy-edit-2427992151.yaml
```

A save that keeps the same name never triggers this check.

## Binding one session, not every run

`policy attach` and `policy detach` take `-n NAME` to bind or unbind one
session instead of every run of a profile. `policy check` takes it to ask
about that one session instead of the profile as a whole.

`-n NAME` must already be the slug form of the name: lowercase letters,
digits, dot, dash and underscore. `attach -n Refactor` is refused outright,
naming the slug it must become. This is stricter than the retired
<!-- retired-ok -->`brig run --name`, which sanitizes a name and reports the
directory it landed on.

The difference is in what you are allowed to type, not in what you get.
`ParseRef` applies the same slug rule to the `<agent>@<label>` form:
`claude@refactor` is accepted and `claude@Refactor` is refused, so a ref
can only ever name a session whose stored name is `refactor`.

A session opened with `brig run claude --name Refactor` <!-- retired-ok -->
reaches the same policy. `brig run` sanitizes `Refactor` to the slug
`refactor` for the sandbox and the workspace, and the policy is looked up
under that slug too, so `brig policy attach locked-down claude-code -n
refactor` covers the session whichever way it was named on the way in.

The session has one identity. The slug names the sandbox and the
workspace, keys the session index, and selects the policy. Whether you
typed `claude@refactor` or `--name Refactor`, you are in session
`refactor` and you get `refactor`'s policy.

`brig policy check -n` reads it the same way, so what it reports is what
the run gets. It stays lenient about the spelling you hand it, unlike
`attach -n`, because `brig policy ls` can print a key an earlier build or
a hand edit left behind and something has to be able to inspect what the
listing names. A row under such a key is reported, and said to be one no
run reaches:

```console
$ brig policy check claude-code -n "My Work"
brig: no-net is recorded under "My Work", which no run reaches
  ↳ a session named "My Work" starts "my-work"
  → to remove it:  brig policy detach no-net claude-code -n "My Work"
no-net
note: enforced on the hvi backend and on Linux with nerdctl, which give the sandbox a network of its own; a run on any other backend is refused rather than left unenforced
```

One gap remains. `attach -n` refuses a name that any reserved profile ends in,
while a session is only refused one reserved for the agent it belongs to. So
`claude@desktop` opens an ordinary session, but
`brig policy attach locked-down claude-code -n desktop` is refused for colliding
with `claude-desktop`. That session cannot be given a policy of its own.

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

`show` prints the parsed document, not the file as you typed it, so the
field order differs: the YAML output sorts keys.

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
| `a policy applies to this sandbox, and hull on vz cannot enforce the egress policy: …` | a policy on a run path whose answer is `cannot enforce`: hull's `vz` or `qemu` backend, or docker. See [Where a policy is enforced](#where-a-policy-is-enforced-and-where-it-is-not) |
| `a policy applies to this sandbox, and whether nerdctl on <shim> enforces the egress policy is unknown: brig cannot find the network namespace of the container network: …` | rootless containerd is not running, `XDG_RUNTIME_DIR` is not set, or `nsenter` is not installed |
| `… is unknown: brig enforces a policy here with nftables: nft is not installed` | `nft` is not installed |
| ``… is unknown: the probe `<nsenter …> /usr/sbin/nft list tables` failed: …`` | nft ran in the namespace and failed, for instance on a kernel without nftables |
| `… is unknown: brig could not start the egress resolver: …` | the resolver could not install the table or bind the bridge address. The error quotes its log |
| `… is unknown: the sandbox's bridge is not where brig put the rules: …` | `BRIG_RUNTIME_BIN` reaches a containerd other than the rootless one Brig found. Brig removes the container |
| `<sandbox> is already running, so brig leaves its egress rules alone` | another `brig` booted the sandbox between this one's check and its boot. Run the command again |
| `a policy applies to this sandbox, and nerdctl on <shim> cannot enforce the egress policy: its rules need a network of its own, and the run asks for the shared network` | a run reached nerdctl with a policy and the `shared` posture. A policy normally narrows the posture to `isolated`, so this is a mismatch between the two. Run it with `--network isolated`. It exits `7` |
| `a policy applies to this sandbox, and hull on hvi cannot enforce the egress policy: the network-gateway of <bin> has no --egress-default. Upgrade the runtime, or detach the policy` | the runtime is older than the hull that added the `--egress-*` gateway flags |
| ``a policy applies to this sandbox, and whether hull on hvi enforces the egress policy is unknown: the probe `<bin> network-gateway --help` failed: …`` | the probe of the runtime did not run, exited non-zero, or gave no answer within 30 seconds |
| `a policy applies to this sandbox, and whether hull on krun enforces the egress policy is unknown: brig holds no answer for this run path. Run it on hull's hvi backend (BRIG_HYPERVISOR=hvi), or detach the policy` | `BRIG_HYPERVISOR` names a backend brig holds no record for, such as `krun`. Brig refuses the run instead of guessing |

## The default is no policy at all

The default egress stance is allow everything, with no filtering applied at all.
It is not a deny-all default, and it is not an empty allow list. A sandbox
nobody attached a policy to has unrestricted egress. No profile Brig ships binds
a policy, and `brig run <agent>` on a fresh install filters nothing. No gateway
is given a rule until a policy is attached to that profile or that session by
hand. The default `isolated` posture gives each new sandbox a network of its
own; it does not filter that sandbox's outbound traffic.

A test holds that default (`TestNoShippedProfileBindsAPolicy`). An agent
that cannot reach its own API does not work, and a deny default would break
every sandbox on upgrade.

Two defaults are easy to confuse. Attaching no policy means no filtering.
Attaching a policy whose `default:` is `deny` means everything is refused
except what its `allow` list names. An empty `allow` list under it is a
sandbox with no way out.

## Where a policy is enforced, and where it is not

Two run paths enforce an egress policy. On macOS it is hull's `hvi`
backend, at the user-mode network gateway Brig gives that sandbox. On Linux
it is nerdctl, at the bridge of the sandbox's own network. `vz` and `qemu`
take their network from vmnet, and docker manages its own bridges. Brig
filters neither.

Brig holds one answer for each run path, a runtime with one backend. The
answer is `enforced`, `cannot enforce` or `unknown`, and it comes from one
table in `internal/runtime/capability.go`. On `hvi` and on nerdctl a probe
at boot confirms the table's answer or overturns it:

| Run path | Egress policy | Why |
|---|---|---|
| hull on `hvi` | `enforced` | the rules go on the gateway that is the sandbox's only way out. The gateway probe confirms it before Brig starts that gateway |
| nerdctl, on any shim | `enforced` | the rules go in nftables on the sandbox's own bridge, and Brig answers its DNS. An nft probe in the bridges' network namespace confirms it before the sandbox boots |
| hull on `vz` | `cannot enforce` | vmnet, which Brig does not filter |
| hull on `qemu` | `cannot enforce` | vmnet, which Brig does not filter |
| docker, on any shim | `cannot enforce` | docker's own bridges and firewall, which Brig does not filter |
| hull on any other backend | `unknown` | Brig holds no answer for it |

Brig boots a policy-bound run only on `enforced`. On `cannot enforce` or
`unknown` it refuses with exit code `7`, and the refusal names the
property, the runtime and the backend. A run with no policy asks nothing,
and a run under `--network offline` asks nothing either.

On the `hvi` backend, a boot reads every policy bound to the run and puts
the rules on the network gateway it gives that sandbox. That gateway is the
sandbox's only way out, so the rules are the sandbox's only way out.
Measured in a real guest in
[docs/manual-tests/egress-policy.md](manual-tests/egress-policy.md): an
allowed name reaches, a denied name does not resolve, and an address
dialled directly does not connect.

On Linux with nerdctl, the same rules are enforced in two halves. See
[How nerdctl enforces a policy](#how-nerdctl-enforces-a-policy) below.
[docs/manual-tests/egress-policy-linux.md](manual-tests/egress-policy-linux.md)
measures it in a urunc guest on rootless nerdctl.

On every backend that cannot enforce a policy, Brig refuses the boot rather
than running unenforced. `vz`, `qemu` and docker refuse a filtered run
outright, naming the backend that does enforce. Refusing it
beats booting a sandbox that reports a policy and filters nothing. There is
one exception: a policy-carrying run whose posture is `offline` is not
refused on any backend. A sandbox with no route out satisfies every rule
set.

That refusal is checked before anything starts, and again on the path that finds
the sandbox already running, so a sandbox that is already up does not skip the
check. Every runtime Brig ships refuses a policy it cannot enforce. This is a
guarantee about the runtimes Brig ships today, not a property of the interface.
A runtime that never answers the question is never asked, and stays unrefused.

The runtime has to be new enough, too. The gateway's `--egress-*` flags
arrived after hull 0.1.0-rc21. Brig probes the binary itself, `<bin>
network-gateway --help`, rather than checking a version number. The exact
release does not matter: a hull newer than 0.1.0-rc21 works. An older one
is refused by name once a policy applies to the run:

```console
$ brig policy attach locked-down claude-code
attached locked-down to claude-code
note: enforced on the hvi backend and on Linux with nerdctl, which give the sandbox a network of its own; a run on any other backend is refused rather than left unenforced
$ brig run claude
brig: a policy applies to this sandbox, and hull on hvi cannot enforce the egress policy: the network-gateway of /opt/homebrew/bin/hull has no --egress-default. Upgrade the runtime, or detach the policy. brig will not boot a sandbox under a policy nothing enforces
```

A probe that fails answers `unknown`, and Brig refuses the boot on that
too. That covers a binary that does not run, a non-zero exit, and no answer
within 30 seconds. A non-zero exit refuses even when the help text lists
`--egress-default`. The refusal names the binary, the probe command and its
error.

### How nerdctl enforces a policy

Brig enforces a policy itself on nerdctl because hull's gateway cannot serve
a Linux guest yet. Once it can, the gateway is where these rules belong, and
the resolver and table below are the interim.

A sandbox under a policy on nerdctl always has a network of its own, and
its traffic leaves through that network's bridge. Before the sandbox boots,
Brig puts two things on that bridge:

- **A resolver**, a `brig` process listening on the bridge address. The
  sandbox boots with that address as its DNS server. Under `default: deny`
  it answers only names an `allow` glob covers, and puts the addresses it
  hands out in the table's allow set for two minutes. Under `default:
  allow` it answers every name, and puts the addresses of a name a `deny`
  glob covers in the deny set. It re-resolves every `host` rule that names
  exactly one host every 30 seconds, and keeps what that returns for 90
  seconds.
- **An nftables table**, `brig_egress_<sandbox>`, in the network namespace
  that holds the bridge. It refuses a connection to an address no rule
  allows: TCP gets a reset, and anything else is dropped. It carries TCP,
  UDP and ping, and refuses every other protocol. It also refuses IPv6,
  `169.254.0.0/16`, and a packet whose source is outside the sandbox's
  network. Rootless nerdctl writes slirp4netns's resolver into the guest's
  `resolv.conf` ahead of Brig's, and the table sends DNS for that address
  to Brig's resolver too. DNS for any other server is ordinary traffic
  under the policy, as it is on `hvi`.

The guest reaches the addresses of that namespace itself for DNS and ping
to the bridge address, and for nothing else. With rootless nerdctl the
namespace is rootlesskit's. The host's own addresses are reached through
slirp4netns there, and the rules treat them like any other address. With
rootful nerdctl the namespace is the host's, so every address of the host
is refused under either default.

The rules mean what they mean on `hvi`. A `host` rule is a glob on the name
the guest asks for, `*` spans dots, and a glob does not cover the bare
domain. Deny beats allow, and allow beats the default. With a `host` rule in
the policy, the lifetimes are the ones hull's gateway uses, and so are the
record types answered. A few details differ, because the gateway answers for
the guest and the resolver passes upstream's answer on:

- The guest gets upstream's answer, with its CNAME chain and any DNSSEC
  records, but with no additional records. The gateway answers with A
  records alone. Either way only the A records of the answer are pinned,
  and the address of an MX or SRV target is learned with an A query, which
  the rules judge.
- An upstream that fails gives `SERVFAIL`. The gateway answers `NXDOMAIN`.
- With no `host` rule in the policy, the TTL is upstream's. The gateway
  answers with a TTL of 0.
- Ping reaches an address the rules allow, and no other. The gateway
  answers every ping itself.
- A refused connection is not logged. A refused name is.

Rootless nerdctl keeps its bridges in rootlesskit's network namespace.
Brig enters it with `nsenter`, as nerdctl does, and needs no privilege for
it. A rootful nerdctl keeps them in the host's namespace. Either way the
host needs `nft` (the nftables package) and `nsenter` (util-linux). Brig
probes for both with `nft list tables` in that namespace before every
filtered boot, and refuses the boot with exit code `7` when the probe
fails.

While the sandbox runs, the table is all that filters it. So `brig stop`
and `brig rm` remove the table and stop the resolver only once the sandbox
is confirmed stopped, and a stop that failed leaves both in place. Each boot
tags the connections it admits, and a connection the kernel still tracks
from an earlier boot is judged again under the new rules.

The resolver keeps the table in place. It installs the table, pins the
addresses of every `host` rule that names one host, and only then starts a
heartbeat in the table, which it renews every 5 seconds. Until the
heartbeat starts, the table refuses every new connection, and Brig boots
the sandbox only after it has. If something removes the table, such as a
`flush ruleset` on a rootful host, the resolver installs it again the same
way within those 5 seconds, and the sandbox is not filtered until it does.
The table installed again holds none of the addresses the guest looked up,
and under `default: deny` those are refused until the guest asks for them
again, within the minute its answers last. If the resolver dies, the
heartbeat lapses within 15 seconds, and from then on the table refuses
every new connection under either default. Connections already open stay
open. The next `brig run` or `brig sh` reboots a sandbox whose resolver
died, as on any other change of rules. A sandbox whose resolver and table
are both gone is not filtered until that boot.

The resolver logs to `~/.brig/egress/<sandbox>.log`, or under
`BRIG_GATEWAY_DIR` when that is set. It logs a refused name once every 30
seconds, at most 20 such lines every 30 seconds, and at most 4 MiB of them
in all. The log starts afresh at each boot and goes with `brig stop`.
Measured in a real guest in
[docs/manual-tests/egress-policy-linux.md](manual-tests/egress-policy-linux.md),
against the cases of the network conformance suite.

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
- **Several policies at once are unioned.** A rule in any of them is a rule of
  the run's. The default is the strictest any of them names: one `deny` makes
  the run deny-by-default. A host the second policy allows is reachable even
  when the first policy alone denies it. At the gateway that enforces the rules,
  `deny` still takes priority over `allow` across the whole set. See the
  precedence note below.

### What this does not do yet

`attach` and `check` refuse what Brig knows it cannot enforce at all (a
`kind: shell`/`kind: gui` profile), and a name bound to nothing. Neither
inspects the rules a policy contains. Neither can tell you that an `allow`
glob matches nothing you meant. They can only tell you that the document
parses.

Brig's own merge applies no priority. It concatenates the `allow` and
`deny` lists from every bound policy. On `hvi`, deny-over-allow-over-default
ordering and the host-rule behavior below are the enforcing gateway's
documented behavior. Each rule reaches the gateway as an `--egress-allow`
or `--egress-deny` flag on its command line. On nerdctl, the ordering is
Brig's own: it is the order of the rules in the table. `internal/egress`
tests the rules it generates, and the manual test below measures them in a
kernel.

[docs/manual-tests/egress-policy.md](manual-tests/egress-policy.md) holds
the measurement on `hvi`. It covers a `default: deny` policy with one
`host` allow: the allowed name reaches, a denied name fails to resolve, and
an address dialed directly fails to connect. It does not exercise a `deny`
rule overriding an `allow` rule, or a `default: allow` policy.
[docs/manual-tests/egress-policy-linux.md](manual-tests/egress-policy-linux.md)
holds the measurement on nerdctl. It runs the conformance suite's cases
under both defaults, including a `deny` glob inside an `allow` glob and a
`deny` cidr inside an `allow` cidr.

A `host` rule is enforced through the resolver Brig puts in front of the
sandbox, the gateway's on `hvi` and Brig's own on nerdctl, so what it
covers depends on the default. Under `default: deny`, the resolver answers
only names an `allow` glob covers, and the guest reaches nothing it did not
resolve there. That is also why traffic sent straight to an address, DNS
over HTTPS and DNS over TLS do not get out. Under `default: allow`, a
`host` deny is best effort, because traffic sent straight to an address
never asks for a name. A `cidr` rule is matched on the address either way.

A `host` rule decides an address, not a name. Neither enforcer reads TLS
SNI or an HTTP `Host` header. Two names served from one address, as on a
CDN, get the same verdict: an `allow` for one lets the guest reach the
other, and a `deny` for one refuses the other.
