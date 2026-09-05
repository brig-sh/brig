# Security policy

brig exists to hold a boundary. An agent gets one directory and the credentials
it was named, and nothing else on the host. Anything that weakens that boundary,
or that reaches something the agent was never given, belongs here rather than in
a public issue.

Tell us privately first and we will fix it with you before it is public. If you
are unsure whether something counts, report it and we will work that out with
you.

## Supported versions

There is no stable release yet. brig ships prereleases only, currently the
`0.1.0-rc` series, and the version you are on is whatever `brig version`
prints. Fixes land on `main` and go out in the next prerelease. We do not
backport to an earlier `rc`, so the supported version is the latest one: if you
are reporting against an older prerelease, please check that the issue still
reproduces on the newest before you send it, and expect the fix to arrive as a
newer prerelease rather than a patch to the one you filed against.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting. Open the advisory page and press
**Report a vulnerability**:

https://github.com/brig-sh/brig/security/advisories

That opens a private thread visible only to you and the maintainers, so nothing
is disclosed while we work on a fix. Please do not open a public issue for a
security problem, and do not send a pull request that reveals the flaw before an
advisory exists.

If you would rather use email, write to **security@brig.sh** instead. Both reach
the same people.

Include what you would want if you were fixing it: the version from
`brig version`, the operating system and runtime (hull on macOS, nerdctl or
runc on Linux), what you did, what happened, and what you expected instead. A
proof of concept helps, even a rough one.

## What to expect

- **First response within 3 working days.** That is an acknowledgement from a
  human that the report arrived and is being looked at, not a fix.
- **A disclosure window of 90 days.** We aim to have a fix released and an
  advisory published within 90 days of the report. If it takes longer we will
  say so in the thread and agree a new date with you rather than let it lapse
  silently. If a fix ships sooner, the advisory goes out sooner. We are happy to
  credit you in the advisory, or to leave you out of it, whichever you prefer.

These are targets a small project can meet, not a contract. If you have not heard
back inside the response window, send a reminder to security@brig.sh.

## Scope

In scope is anything that breaks a promise brig actually makes: a run that
reaches a host credential it was not given or a file outside the workspace, a
credential that leaks off the intended channel, the secret store handing back an
item it should not, image verification passing something it should reject, or a
tampered release verifying as genuine. `CONTRIBUTING.md` calls the first two the
two promises, and `docs/security.md` is where the exact edges of all of them are
written down.

Out of scope are the limitations brig already declares. `docs/security.md` lists
them under
[Things brig does not claim](docs/security.md#things-brig-does-not-claim): brig
does not sandbox the agent from the network, does not isolate one sandbox from
another, does not filter terminal escape sequences the agent writes, and does
not stop an agent misusing a credential it was deliberately handed. A report
that brig does one of those is describing a known limitation, not a
vulnerability. That page is the authority on the boundary, so we point at it
here rather than restate it, which keeps the two from drifting apart. If you
think one of those limitations is worse than the page admits, or the page is
wrong about where a line sits, that is worth a report.
