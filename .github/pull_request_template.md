<!--
Title: use a conventional-commit subject, e.g. fix(wrap): refuse a symlinked workspace
Example scopes: wrap, profile, creds, secret, runtime, verify, session, brigd, ci, docs.
-->

## Summary

<!-- What this changes and why. Lead with the problem, then the approach. -->

## Related issues

<!-- Closes #NN, Refs #MM. Delete if none. -->

## Changes

<!-- The notable changes, one bullet each. -->

-

## Checklist

<!--
Check items as you complete them; strike through (~~like this~~) any that do
not apply, rather than deleting or rewording them. Keep the reasoning in
Summary or Changes, not here.

`make all` is vet + test + build, which is what CI gates on. `script/smoke.sh`
drives the real binary end to end against a stub runtime and a stub cosign, so
it runs anywhere -- no VM and no macOS needed. See CONTRIBUTING.md.

brig drives a runtime it does not own: hull on macOS, nerdctl on Linux. A
change to how brig invokes either can pass every test here and still be wrong,
which is what the fifth box is for.
-->

- [ ] `make all` passes (vet, test, build)
- [ ] `script/smoke.sh` passes
- [ ] I have added or updated tests covering the change
- [ ] I have run `go test ./... -race`, if the change touches concurrency, subprocesses or the daemon
- [ ] I have exercised the change against a real runtime (`brig run <agent>`), if it touches the run, exec or credential path
- [ ] I have updated the affected docs (README, `docs/security.md`, `docs/profiles.md`, `docs/brigd.md`)

## Credentials and the sandbox boundary

<!--
Delete this section if the change touches neither.

brig makes two promises: the guest sees one directory, and it gets the
credentials it was named and no others. Anything touching internal/wrap,
internal/creds, internal/profile or internal/secret should say which promise it
affects and how that was checked -- ideally with a test that fails without the
change. A negative test is worth more here than a positive one: "the denied
variable was not forwarded" and "the planted symlink was refused" are the
assertions that catch a regression.
-->
