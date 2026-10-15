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
- `macos:TestName` is a Go test that runs on macOS alone, found as a `go:`
  test is.
- `macos-smoke:<text>` is an assertion in the part of `script/smoke.sh` that
  runs on macOS alone, found as a `smoke:` assertion is.

Any other token, a row with no token, or text beside the tokens fails the
check.

Every line after the table's delimiter row is a row, up to the first blank
line, with or without its outer pipes. A line there that is not three cells
fails the check. A pipe line outside the table fails it too.

## Checks that need a Mac

CI runs on Linux. There a `macos:` test skips, and smoke never reaches a
`macos-smoke:` assertion. The CI check resolves both by name and lists them
as not run in CI. Before a merge that touches the host kind or the broker,
run `go test ./...` and `script/smoke.sh` on a Mac.

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

The rows under any section but The process boundary describe the microVM
boundary. The `host` kind runs the agent as a process, and those rows do not
hold for it. Its own rows are the ones under The process boundary.

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
| "The process gets the variables the profile delivers, and from the environment Brig runs in only the ones runtimes.md lists, such as `PATH` and `TERM`." | The process boundary | `go:TestHostEnvForwardsOnlyWhatIsNamed` `macos-smoke:a variable brig was not told to forward stays out of it` |
| "No value is on its command line." | The process boundary | `macos:TestHostArgvCarriesNoValue` `macos-smoke:a credential value never reaches sandbox-exec's argv` |
| "On `isolated`, no IP connection leaves this Mac, and a local address is reachable only on the ports Brig names." | The process boundary | `go:TestSandboxProfileNetworkAllows` `macos:TestHostSandboxDeniesWhatItShould` |
| "On `offline`, nothing is reachable." | The process boundary | `macos:TestHostSandboxOfflineReachesNothing` |
| "The process cannot listen for a connection." | The process boundary | `macos:TestHostSandboxCannotListen` |
| "The process cannot start an app through LaunchServices." | The process boundary | `macos:TestHostSandboxCannotOpenAnApp` |
| "The process cannot send a signal to a process outside it." | The process boundary | `go:TestSandboxProfileAlwaysDeniesTheSensitivePaths` `macos:TestHostSandboxDeniesWhatItShould` |
| "The process cannot write Brig's own profiles, policies or state outside its guest home, and cannot read or rewrite the profile it runs under." | The process boundary | `macos:TestHostSandboxHoldsBrigsOwnFiles` `macos:TestHostSandboxDeniesWhatItShould` |
| "The process cannot ask the keychain for a secret, or connect to a unix socket other than the system resolver's, your SSH agent's included." | The process boundary | `macos:TestHostSandboxDeniesTheKeychain` `macos:TestHostSandboxDeniesWhatItShould` |
| "A secret a route injects never reaches the process, its environment or its guest home." | The process boundary | `macos:TestTheHostKindReachesItsRouteAndNothingElse` `go:TestAHostRunBoundToARouteGetsATokenAndThePort` `go:TestABrokeredSecretIsNeverDelivered` |
| "A session token works only on its own sandbox's route, and stops working when `brig stop`, `brig rm` or `brig rm --all` removes the sandbox." | The process boundary | `macos:TestTheHostKindReachesItsRouteAndNothingElse` `go:TestATokenOfAnotherSandboxIsRefusedOverLoopback` `go:TestControlProtocolRegistersMintsAndRevokes` `go:TestStopAndRemoveRevokeTheSandboxTokens` `go:TestRemoveAllRevokesEverySandboxsTokens` `macos-smoke:the host sandbox reaches the broker, which refuses a token it did not mint` |
| "The process cannot read the broker's directory, which holds the host key and the key of the brig CA." | The process boundary | `macos:TestTheHostKindReachesItsRouteAndNothingElse` `go:TestTheBrokerRefusesADirectoryOrKeyOthersCanReach` |
| "No session token is on a command line, and the audit log holds no token and no credential." | The process boundary | `macos:TestTheHostKindReachesItsRouteAndNothingElse` `go:TestTheAuditLogHoldsNoValueOrToken` `go:TestAuditLineOfAnUnverifiedRequestCarriesNoPath` `macos-smoke:nothing reached a credential in argv: the session token stays out of sandbox-exec's` |
