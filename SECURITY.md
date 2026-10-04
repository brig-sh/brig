# Security policy

An agent gets the host directories and the credentials Brig names for it, and
nothing else on the host. Report privately anything that weakens that
boundary, or that reaches something the agent was never given. Do not open a
public issue for it.

Tell us privately first and we will fix it with you before it is public. If
you are not sure that something counts, report it and we will work that out
with you.

## Supported versions

The supported version is the latest release. `brig version` prints the
version you run. Fixes land on `main` and go out in the next release. We do
not backport, so a fix arrives as a newer release and not as a patch to the
version in your report. If you report against an older version, first make
sure that the problem still occurs on the latest release.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting:

1. Open the advisory page: https://github.com/brig-sh/brig/security/advisories
2. Press **Report a vulnerability**.

That opens a private thread that only you and the maintainers can see, so
nothing is disclosed while we work on a fix. Do not open a public issue for a
security problem. Do not send a pull request that reveals the flaw before an
advisory exists.

To use email instead, write to **security@brig.sh**. Both reach the same
people.

Include what a fix needs:

- the version from `brig version`
- the operating system
- the runtime: hull on macOS, or nerdctl with the urunc shim on Linux. If
  `BRIG_CONTAINERD_RUNTIME` names another shim, say which.
- what you did, what happened, and what you expected instead
- a proof of concept, if you have one. A rough one helps.

## What to expect

- **First response within 3 working days.** A person acknowledges that the
  report arrived and that someone is looking at it. The response is not a
  fix.
- **A disclosure window of 90 days.** We aim to release a fix and publish an
  advisory within 90 days of the report. If that takes longer, we say so in
  the thread and agree a new date with you. If a fix ships sooner, the
  advisory goes out sooner.
- **Credit as you prefer.** We credit you in the advisory, or leave you out
  of it.

These are targets a small project can meet, not a contract. If you get no
answer inside the response window, send a reminder to security@brig.sh.

## Scope

In scope is anything that breaks a promise Brig makes:

- a run that reaches a host credential it was not given, or a host path
  outside the directories Brig names for it
- a credential that leaks off the intended channel
- the secret store handing back an item it must not
- image verification passing something it must reject
- a tampered release verifying as genuine

`CONTRIBUTING.md` calls the first two the two promises.
[docs/security.md](docs/security.md) states the edges of all of them, and it
is the authority on the boundary.

Out of scope are the limitations Brig already declares. `docs/security.md`
lists them under
[Things brig does not claim](docs/security.md#things-brig-does-not-claim):

- Brig does not sandbox the agent from the network by default.
- Brig does not isolate one sandbox from another on the `shared` network.
  That includes an older session that kept `shared`, and a `vz` or `qemu`
  backend fallback. The postures that do isolate are `isolated` (the default
  for new `hvi` and Linux sandboxes) and `offline`.
- Brig does not filter terminal escape sequences the agent writes.
- Brig does not stop an agent misusing a credential you delivered to it.

A report that Brig does one of those describes a known limitation, not a
vulnerability. If you think one of those limitations is worse than
`docs/security.md` states, or that the page puts a line in the wrong place,
report it.
