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

## Rows checked at the command line only

The rows for moving a session, from "RAM with credentials leaves the host
only through ssh to the host you named." to "After a move, an exec on the
destination carries only the credentials that host's own store and
environment hold.", are defended by tests against a stub hull and a fake
ssh.
Those tests check every command line a move runs and every record it keeps.
They do not show what a real ssh and a real hull do with those lines, and CI
runs no move. On 2026-10-14 moves committed between two Raspberry Pi 5 hosts,
there and back, over real ssh, by hand and with `script/teleport-demo.sh`:
see [On real hardware](teleport.md#on-real-hardware). Those runs saw the
policy row's refusal (exit 7, nothing paused), and an offline session, with
no policy, move with no gateway on either host. They checked no credential:
the sessions had none. The
rows are still defended only at the command line; the runs are not repeated
by any check here.

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
| "Brig shares neither a guest home nor a project into it, so no host directory is mounted at all." | What the agent can reach | `go:TestRunArgsForATeleportableSessionShareNothing` `go:TestEnsureRunningBootsATeleportableSessionWithNoShare` `smoke:a teleportable boot passes no --shared-dir` `smoke:a teleportable run makes no guest home on the host` |
| "A teleportable session's disk holds no credential value Brig resolved." | What the agent can reach | `go:TestRunArgsForATeleportableSessionCarryNoCredential` `go:TestBindMarksEveryRefResolvedVarACredential` `go:TestATeleportableBootHandsTheRuntimeItsCredentialsMarked` `go:TestATeleportableBootLeavesHullItsOwnEnvironment` `go:TestTeleportableRefusesAProfileThatReadsCredentialsAtBoot` `smoke:a teleportable run line names no credential and hands hull none` `smoke:the agent's exec on a teleportable session carries GH_TOKEN` |
| "RAM with credentials leaves the host only through ssh to the host you named." | What the agent can reach | `go:TestSSHArgvNamesOnlyTheHost` `go:TestCheckRemote` `go:TestMigrateArgs` `go:TestAMoveIsNotHandedAnotherTransport` `go:TestATeleportThatCommitsRecordsTheHost` `smoke:the move goes over ssh to the named host and no other` |
| "A session on a network and under an egress policy is moved only to a host whose gateway can enforce one, and is refused before anything is paused otherwise." | What the agent can reach | `go:TestCheckProbeRefusesEachReason` `go:TestATeleportRefusedBeforeAnythingIsPaused` `smoke:a policy the destination cannot enforce is refused with exit 7` `smoke:the refused move paused nothing and reserved nothing there` |
| "An offline session has no gateway on either host, and its rules move with it as its recorded policy." | What the agent can reach | `go:TestCheckProbeRefusesEachReason` `go:TestAnOfflineArrivalKeepsItsRulesWithNoGateway` `smoke:an offline session under a policy moves with no gateway there` `smoke:the brig there runs the offline session under the rules it carries` |
| "After a move, an exec on the destination carries only the credentials that host's own store and environment hold." | What the agent can reach | `go:TestAnArrivedSessionRunsWithoutASecretThisHostLacks` `go:TestAMovedSessionsVerbsRunOnItsHost` `smoke:no credential value reaches an ssh line` `smoke:sh hands no credential from this host to ssh` `smoke:the exec on the destination carries no credential from this host` |
| "The guest gets only the credentials you deliver to it." | What the agent can reach | `smoke:an undeclared ambient variable and its value reach no runtime argv or env line` `smoke:an undeclared ambient variable stays out under an override too` `smoke:the declared credential name reaches the guest` |
| "`--home` pointed at a symlink is refused for the same reason, with the same kind of message, and is fixed by naming the real directory." | Writing into the workspace | `go:TestWorkspaceStillRefusesASymlinkAtTheWorkspace` `go:TestSymlinkedWorkspaceRootIsRefused` `go:TestWorkspaceRefusesASymlinkedParentComponent` |
| "The one case with no innocent reading is an image sitting under our registry whose signature does not verify. That is the case that stops." (an image under `ghcr.io/brig-sh/`) | Guest images | `smoke:a bad signature on our own image is reported` `smoke:a bad signature stops the boot (exit 5) with no terminal to ask` `go:TestVerifyRefusesAFailedSignatureWithNoTerminal` `go:TestVerifyRefusesAFailedSignatureWithStdinOnDevNull` |
| "A `scheme://` value read from the environment is refused as an unresolved secret-manager reference." | Credentials | `smoke:a secret-manager reference is not forwarded` `go:TestUnresolvedReferencesAreRejectedButOrdinaryURLsAreNot` `go:TestEnvRefsStillGetTheUnresolvedRefGuard` |
| "Brig compares the portable kernel with the sha256 that Brig pins for it before the boot." | The kernel, not only the image | `go:TestThePortableKernelIsCheckedAgainstThePinAndTheInitrdAgainstTheBundle` `go:TestAPortableKernelThatIsNotThePinnedBuild` `go:TestThePortableKernelIsCheckedWithNoBundleToBind` `go:TestAnUnreadablePortableKernelWarnsOrRefuses` `go:TestATeleportableProfileWithoutGenericBootIsRefusedOnAPortableHost` `go:TestABootWhoseResolveNamesNoMachineIsRefused` `go:TestRunArgsRefusesAMachineWithNoBootAssets` |
| "The object cosign checked is the object that runs, and the success line names the digest instead of the tag it came from." (the object that runs) | Digest pinning | `go:TestVerifyResolvesVerifiesAndPinsAMatchingDigest` `go:TestRunArgsBootsThePinnedDigest` `go:TestNerdctlBootsTheVerifiedDigest` `smoke:the verified digest is what hull was told to boot` |
