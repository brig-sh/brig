# Claims and the tests behind them

Each row in [the table](#the-table) quotes one sentence from
[security.md](security.md) and names the tests that defend it. security.md
states what the guest can and cannot reach.

`script/check-claims.sh` reads the table on every pull request. It fails when
a quoted sentence is gone from security.md, or when a named test no longer
exists. `make claims` runs the same check after its self-test.

## Reading a row

A row has three cells.

**Claim.** The cell quotes security.md. The check matches only the part in
double quotes. A note in parentheses after the quote says which part of the
sentence the row covers. Quote the whole sentence, through its period. Then a
qualifier added to the sentence in security.md ends the match. security.md
wraps its prose, so the match ignores line breaks and runs of spaces.

**Section.** The cell names the heading the sentence is under. The quote must
be under that heading or under a heading nested in it.

**Defended by.** Each defence is one token:

- `go:TestName` is a Go test, found as `func TestName(` in a `_test.go` file.
- `smoke:<text>` is an assertion in `script/smoke.sh`, found as its
  `ok "<text>"` line.
- `vm:<check>` is a check in `script/claims-vm.sh`, found as its
  `vm_check <check>` line.

Any other token, a row with no token, or text beside the tokens fails the
check.

Every line after the table's delimiter row is a row, up to the first blank
line, with or without its outer pipes. A line there that is not three cells
fails the check. A pipe line outside the table fails it too.

## Checks that need a sandbox

A `vm` check needs a booted sandbox, and CI has no runtime. The CI check
resolves a `vm` row by name and lists it as not yet run.

Before a merge that touches the run path, run `make claims-vm`. It builds
`brig` from this checkout and runs the `vm` checks against that binary where
hull or nerdctl is on `PATH`. It skips where neither is. Run by hand,
`script/claims-vm.sh` tests the `brig` on `PATH` unless `BRIG` names another.

CI does run `script/claims-vm.sh --self-test`. The self-test answers every
check from a fake guest that leaks one thing at a time, and each check must
fail on its own leak. A few checks also run their real probes through a stub
`brig` on the host, so a probe with no answer fails too. The self-test proves
that each check judges an answer correctly. Only a booted sandbox proves that
the guest has none.

## The table

| Claim | Section | Defended by |
| --- | --- | --- |
| "Beyond those, the guest does not have your keychain, your SSH agent, your secret manager, or any other directory on the host." (your keychain) | The boundary | `go:TestTheRunPathReadsNoKeychain` `vm:keychain-not-reachable` `vm:secret-service-not-reachable` |
| "Beyond those, the guest does not have your keychain, your SSH agent, your secret manager, or any other directory on the host." (your secret manager) | The boundary | `go:TestTheRunPathCannotReachTheImporter` `go:TestUnresolvedReferencesAreRejectedButOrdinaryURLsAreNot` |
| "Beyond those, the guest does not have your keychain, your SSH agent, your secret manager, or any other directory on the host." (your SSH agent) | The boundary | `vm:ssh-agent-not-forwarded` `vm:no-agent-socket` |
| "Beyond those, the guest does not have your keychain, your SSH agent, your secret manager, or any other directory on the host." (any other host directory) | The boundary | `vm:other-host-directory` |
| "Forwarded values go into the runtime process's own environment, and only the variable *name* appears on its command line." | Not in argv | `go:TestSplitEnvKeepsValuesOutOfArgv` `go:TestRunArgsKeepsSecretValuesOutOfArgv` `smoke:credential values reach the runtime, but never through argv` `smoke:argv names the variables only` |
| "Nothing is written into the guest home from the host for this." | Credentials | `smoke:no credential is written into the workspace` |
| "So every host-side read and write Brig makes inside the guest home goes through an `os.Root` opened on it." | Writing into the workspace | `go:TestMarkerWriteRefusesAPlantedSymlink` `go:TestSetupGitRefusesASymlinkedGitconfig` |
| "A variable on the profile's `deny` list is refused, with the reason." | Credentials | `go:TestDenyAppliesToRefdValues` `go:TestOffSpellingsDoNotForwardADeniedCredential` `smoke:the metered key is refused, and says why` `smoke:a denied key never reaches the guest env line of a run` `smoke:a denied key's value never reaches argv, even under BRIG_ENV_ARGV=1` |
| "Inside Brig, the guest has your guest home mounted as its home, read-write." | The boundary | `smoke:the workspace is mounted as the guest home` `vm:guest-home-read-write` |
| "Name a project on the run line and that project is a second host directory, also mounted read-write, at `/work/<name>`." | The boundary | `smoke:the project is mounted at /work/<basename>` `vm:project-at-work` |
| "The guest gets only the credentials you deliver to it." | What the agent can reach | `smoke:an undeclared ambient variable and its value reach no runtime argv or env line` `smoke:an undeclared ambient variable stays out under an override too` `smoke:the declared credential name reaches the guest` |
| "`--home` pointed at a symlink is refused for the same reason, with the same kind of message, and is fixed by naming the real directory." | Writing into the workspace | `go:TestWorkspaceStillRefusesASymlinkAtTheWorkspace` `go:TestSymlinkedWorkspaceRootIsRefused` `go:TestWorkspaceRefusesASymlinkedParentComponent` |
| "The one case with no innocent reading is an image sitting under our registry whose signature does not verify. That is the case that stops." (an image under `ghcr.io/brig-sh/`) | Guest images | `smoke:a bad signature on our own image is reported` `smoke:a bad signature stops the boot (exit 5) with no terminal to ask` `go:TestVerifyRefusesAFailedSignatureWithNoTerminal` `go:TestVerifyRefusesAFailedSignatureWithStdinOnDevNull` |
| "A `scheme://` value read from the environment is refused as an unresolved secret-manager reference." | Credentials | `smoke:a secret-manager reference is not forwarded` `go:TestUnresolvedReferencesAreRejectedButOrdinaryURLsAreNot` `go:TestEnvRefsStillGetTheUnresolvedRefGuard` |
| "The object cosign checked is the object that runs, and the success line names the digest instead of the tag it came from." (the object that runs) | Digest pinning | `go:TestVerifyResolvesVerifiesAndPinsAMatchingDigest` `go:TestRunArgsBootsThePinnedDigest` `go:TestNerdctlBootsTheVerifiedDigest` `smoke:the verified digest is what hull was told to boot` |
