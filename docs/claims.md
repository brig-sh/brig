# The claims suite

[docs/security.md](security.md) makes specific, testable promises about where
brig's boundaries are. A promise that nothing checks holds only until a refactor
quietly ends it, and several of these have regressed or nearly regressed. So
each public claim gets one test, and this table is the link between the two: a
claim with no test is a row with no test named, visible in review.

`script/check-claims.sh` reads this table on every merge and fails the build
when a row names a test that does not exist. That is the guard that keeps the
table honest as the suite moves under it.

## How to read the Defended-by column

Each test is named with a typed token so the checker can find it:

- go:TestName is a Go test, checked against `go test -list ./...`.
- smoke:&lt;text&gt; is a named check in `script/smoke.sh`, matched as its
  `ok "<text>"` line.
- vm marks a claim only a real VM can prove. It is defended by the
  `make claims-vm` target, which boots a real sandbox where a runtime exists and
  skips cleanly where none does. A stub cannot prove isolation, so nothing here
  pretends it can.
- pending marks a claim whose test lives on a branch not yet merged. The row
  exists so the claim is tracked; the checker skips it until the test lands.
  Read pending as "the fix is written, on an unmerged branch" rather than
  "unknown": where a pending row also records that the current behaviour is
  wrong, that is called out in the row itself, so pending is never mistaken for
  a passing claim.

## The table

| Claim (exact sentence from the docs) | Where it appears | Defended by | Status |
| --- | --- | --- | --- |
| "the agent sees one directory and the credentials you chose to give it, and nothing else on the host." | security.md, intro | `vm` | needs a VM |
| "It does not have your keychain, your SSH agent, your secret manager, or any other directory on the host." | security.md, The boundary | `vm` | needs a VM |
| "Inside it, the guest has your workspace mounted as its home." | security.md, The boundary | `smoke:the workspace is mounted as the guest home` | existing |
| "`--workspace` pointed at a symlink is refused for the same reason, with the same kind of message" | security.md, Writing into the workspace | `go:TestWorkspaceStillRefusesASymlinkAtTheWorkspace` + `go:TestSymlinkedWorkspaceRootIsRefused` | existing |
| "So every host-side read and write brig makes inside the workspace goes through an `os.Root` opened on the workspace." (a symlink on the way to the workspace is refused) | security.md, Writing into the workspace | `go:TestWorkspaceRefusesASymlinkedParentComponent` | existing |
| "A symlink that stays *inside* the workspace is refused too: brig writes only regular files in there" (a planted symlink is not followed by a host-side write) | security.md, Writing into the workspace | `go:TestMarkerWriteRefusesAPlantedSymlink` + `go:TestSetupGitRefusesASymlinkedGitconfig` | existing |
| "stops and asks `[y/N]`; refuses when there is no terminal" (image under `ghcr.io/brig-sh/` with a failed signature) | security.md, Guest images (decision table) | `smoke:a bad signature stops the boot with no terminal to ask` + `go:TestVerifyRefusesAFailedSignatureWithNoTerminal` | existing |
| "Forwarded values go into the runtime process's own environment, and only the variable *name* appears on its command line." (no credential value in argv) | security.md, Not in argv | `smoke:credential values reach the runtime, but never through argv` + `go:TestSplitEnvKeepsValuesOutOfArgv` + `go:TestRunArgsKeepsSecretValuesOutOfArgv` | existing |
| "the agent sees one directory and the credentials you chose to give it, and nothing else on the host." (only the declared credential names reach the guest) | security.md, intro; CONTRIBUTING.md, The two promises | `smoke:the declared credential name reaches the guest` + `smoke:only declared names reach the guest, ambient ones are dropped` | new |
| "The `deny` list keeps a metered key from being forwarded by accident" (a denied billing key does not reach the guest) | security.md, Things brig does not claim | `smoke:the metered key is refused, and says why` + `smoke:a denied billing key never reaches the guest env line` + `smoke:the denied key's value never reaches argv` + `go:TestDenyAppliesToRefdValues` | existing + new |
| "A `scheme://` value read from the environment is refused as an unresolved secret-manager reference." | security.md, Credentials (the three rules) | `smoke:a secret-manager reference is not forwarded` + `go:TestUnresolvedReferencesAreRejectedButOrdinaryURLsAreNot` | existing |
| "Nothing is written into the workspace." (no credential lands in the workspace) | security.md, Credentials | `smoke:no credential is written into the workspace` | existing |
| "`BRIG_ALLOW_DENIED=false` does not forward the denied variable" (the fail-open switch) | security.md, Credentials (on an unmerged branch) | `pending:BRIG_ALLOW_DENIED false path, on a branch not yet merged` | pending (fixed on #63, not merely untested: on this branch `BRIG_ALLOW_DENIED=false` still forwards the denied variable, so the claim is currently false; #63 is the fix) |
| "the digest that booted is the digest that was verified" | security.md, Guest images (pending the digest PR) | `pending:digest pinning, pending the digest PR` | pending |
