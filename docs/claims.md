# Claims and the tests behind them

Each row in [the table](#the-table) quotes one sentence from
[security.md](security.md) and names the tests that defend it. security.md
states what the guest can and cannot reach. Where a promise there rests on
detail another page holds, such as [broker.md](broker.md),
[runtimes.md](runtimes.md) or [secrets.md](secrets.md), a row quotes that
page instead.

`script/check-claims.sh` reads the table on every pull request. It fails when
a quoted sentence is gone from its page, or when a named test no longer
exists. `make claims` runs the same check after its self-test.

## Reading a row

A row has three cells.

**Claim.** The cell quotes the page. The check matches only the part in
double quotes. A note in parentheses after the quote says which part of the
sentence the row covers. Quote the whole sentence, through its period. Then a
qualifier added to the sentence on the page ends the match. A list item or a
table cell with no period is quoted whole. The pages wrap their prose, so
the match ignores line breaks and runs of spaces.

**Section.** The cell names the heading the sentence is under. The quote must
be under that heading or under a heading nested in it. A sentence of a page
other than security.md names its page first, with a colon, such as
`broker.md: Placeholder detection`. The page is in `docs/`, and is not
claims.md. A page's `#` title is not a section, since it holds the whole
page.

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
as not run in CI. Before a merge that touches the broker, run
`go test ./...` and `script/smoke.sh` on a Mac.

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
| "An image under our registry whose signature does not verify stops the boot, under `warn` too." (an image under `ghcr.io/brig-sh/`) | Guest images | `smoke:a bad signature on our own image is reported` `smoke:a bad signature stops the boot (exit 5) with no terminal to ask` `go:TestVerifyRefusesAFailedSignatureWithNoTerminal` `go:TestVerifyRefusesAFailedSignatureWithStdinOnDevNull` |
| "A `scheme://` value read from the environment is refused as an unresolved secret-manager reference." | Credentials | `smoke:a secret-manager reference is not forwarded` `go:TestUnresolvedReferencesAreRejectedButOrdinaryURLsAreNot` `go:TestEnvRefsStillGetTheUnresolvedRefGuard` |
| "The object cosign checked is the object that runs, and the success line names the digest instead of the tag it came from." (the object that runs) | Digest pinning | `go:TestVerifyResolvesVerifiesAndPinsAMatchingDigest` `go:TestRunArgsBootsThePinnedDigest` `go:TestNerdctlBootsTheVerifiedDigest` `smoke:the verified digest is what hull was told to boot` |
| "`brig info --json` names the channel of each credential as its `source`: `environment`, `secret`, `file` or `broker`." (`broker`) | Credentials | `go:TestEnvelopeHasABrokerRow` |
| "A request that fails one step gets the error of that step, and the upstream is never dialed." | broker.md: Request path | `go:TestTokenOfAnotherSandboxIsRefused` `go:TestTokenForAnotherRouteIsRefused` `go:TestPlaceholderInEveryPositionAndEncodingIsRefused` `go:TestRequestAllowMissIsRefusedBeforeResolve` `go:TestNonCanonicalPathIsRefused` `go:TestUpgradeIsRefused` |
| "every request trailer. The rules here edit the headers, and a trailer would carry past them what they remove" | broker.md: Headers | `go:TestRequestTrailersAreNotForwarded` |
| "drops every `Set-Cookie`" | broker.md: Response | `go:TestReflectedCredentialIsRedacted` |
| "drops every header and trailer whose value holds an injected string" | broker.md: Response | `go:TestReflectedCredentialIsRedacted` |
| "replaces each injected string in the body with `[redacted]`, as the body streams" | broker.md: Response | `go:TestReflectedCredentialIsRedacted` `go:TestRedactedEventStreamIsNotBuffered` |
| "So after the token verifies, and before `request.allow` and the credential, the broker scans the request for every live token of the sandbox." | broker.md: Placeholder detection | `go:TestPlaceholderInEveryPositionAndEncodingIsRefused` `go:TestPlaceholderInAHeaderNameIsRefused` `go:TestPlaceholderInTheMethodIsNotAudited` `go:TestTokensStayLiveUntilTheyExpire` |
| "Sent as the carrier of the other route, it fails the route check with 401 before the scan." | broker.md: Placeholder detection | `go:TestPlaceholderOfAnotherRouteIsRefused` `go:TestTokenForAnotherRouteIsRefused` |
| "Each request carries the sandbox whose socket it arrived on." | broker.md: Transports | `go:TestBrigInternalRefusesATokenOnAnotherSandboxsSocket` |
| "The arrival check refuses a request for another sandbox with `404` and `check=arrival`, before the sandbox is looked up or a token is read, so a guest cannot tell another sandbox from one that does not exist." | broker.md: Transports | `go:TestBrigInternalRefusesATokenOnAnotherSandboxsSocket` `go:TestAnotherSandboxsPathReadsAsUnknownFromThisSocket` |
| "A sandbox registered for `brig.internal` is refused on the loopback port the same way." | broker.md: Transports | `go:TestBrigInternalRefusesATokenOnAnotherSandboxsSocket` |
| "The key never leaves the broker." | broker.md: Transports | `go:TestTheCAHandedOverHoldsNoKey` `go:TestTheBrigCAIsCheckedBeforeTheGuestGetsIt` `go:TestTheBrigCAIsDeliveredWithoutItsKey` `macos-smoke:an endpoint run on hvi routes brig.internal to the broker and hands the guest the CA alone` |
| "A diverted connection to it is served as `brig.internal` and never dialed." | broker.md: Transports | `go:TestBrigInternalIsServedOnTheDivertSocket` `go:TestBrigInternalServesTLSOnPort443Alone` |
| "It refuses by closing the socket, and the diverter then resets the guest's connection." | broker.md: Transports | `go:TestDivertClosesOnABadPreamble` `go:TestBrigInternalServesTLSOnPort443Alone` |
| "A connection over either is closed." | broker.md: Transports | `go:TestDivertCapsConnectionsAcrossSandboxes` |
| "The table refuses every other port of the bridge address, except DNS." | policies.md: Routing brig.internal | `go:TestRulesetDivertsBrigInternalOnPort443Alone` |
| "A policy that names another mode still parses." | broker.md: Route modes | `go:TestRouteValidateLeavesAnotherModeToTheBuild` `go:TestAPresetRefusesAModeItDoesNotList` |
| "`brig policy check` refuses the route, and a run refuses it before boot with exit `7`:" | broker.md: Route modes | `go:TestCheckRefusesARouteInAModeNotInThisBuild` `go:TestARouteInAModeNotInThisBuildIsRefused` `macos-smoke:policy check refuses a route in a mode this build does not serve, with stdout one name per line` `macos-smoke:a run bound to a route in a mode this build does not serve is refused with exit 7` `macos-smoke:nothing reached the runtime for a run refused for its route mode` |
| "A route in mode `endpoint` is refused where its run path cannot reach the broker:" | broker.md: Route modes | `go:TestARunBoundToARouteIsRefusedBeforeBoot` `go:TestCheckRefusesAnEndpointRouteItCannotServe` `go:TestCheckBrigInternal` `go:TestHullRefusesBrigInternalOffTheGateway` `macos-smoke:policy check refuses an endpoint route on vz, naming why` `macos-smoke:a run bound to a route on vz is refused with exit 7, naming why` |
| "`GET /status` on the control socket counts the refusals per sandbox." | broker.md: Placeholder detection | `go:TestPlaceholdersAreCounted` `go:TestStatusCountsPlaceholderRefusals` |
| "A carrier that holds another token is refused." | broker.md: Placeholder detection | `go:TestCarrierHoldingAnotherTokenIsRefused` |
| "the body, up to 8 MiB, and the trailers of a body read to its end. The trailers are not forwarded" | broker.md: Placeholder detection | `go:TestPlaceholderEndingAtTheCapIsFound` `go:TestPlaceholderInATrailerIsRefused` `go:TestRequestTrailersAreNotForwarded` |
| "It looks for each token in these spellings:" (`raw`, `percent`, `json`, `hex` and `base64`, each in upper and lower case, in the path, the query, a header and the body) | broker.md: Placeholder detection | `go:TestPlaceholderInEveryPositionAndEncodingIsRefused` `go:TestBase64AtEveryAlignmentIsFound` `go:TestSpellingsTheScanFindsAndMisses` |
| "So a token split across lines is found, such as base64 wrapped at 76 columns, also inside a JSON string." | broker.md: Placeholder detection | `go:TestPlaceholderInEveryPositionAndEncodingIsRefused` `go:TestSpellingsTheScanFindsAndMisses` `go:TestScanCarriesAnEscapeAcrossWrites` |
| "A gzip or deflate body is scanned as it decompresses, and the encoding is named `gzip+raw` and so on." | broker.md: Placeholder detection | `go:TestCompressedBodies` |
| "A sandbox that is removed takes its tokens and its count with it." | broker.md: Placeholder detection | `go:TestLiveTokensAreBoundedAndPruned` |
| "The tokens of other sandboxes are not looked for." | broker.md: Placeholder detection | `go:TestTokenOfAnotherSandboxInTheBodyPasses` |
| "A body the budget cannot hold is never forwarded unscanned." | broker.md: Holding the body | `go:TestBodyTheBudgetCannotHoldIsRefused` |
| "So one sandbox that stalls its bodies leaves the rest of the budget to the others." | broker.md: Holding the body | `go:TestOneSandboxCannotDrainTheBodyBudget` `go:TestASandboxHoldsAtMostItsShareOfTheBudget` |
| "Each sandbox has its own key, so a token of one sandbox fails the signature check on the path of another." | broker.md: Session tokens | `go:TestSandboxKeysDiffer` `go:TestTokenOfAnotherSandboxIsRefused` `go:TestATokenOfAnotherSandboxIsRefusedOverLoopback` |
| "Removing a sandbox from the broker refuses its tokens from the next request on, with 404." | broker.md: Session tokens | `go:TestRemovedSandboxTokensAreRefused` `go:TestControlProtocolRegistersMintsAndRevokes` |
| "A sandbox added again under the same id has a new nonce and a new key, so the tokens minted before the removal fail the signature check with 401." | broker.md: Session tokens | `go:TestReAddedSandboxDoesNotReviveOldTokens` `go:TestReRegisteringKeepsTokensAndChangingRoutesRevokesThem` |
| "`brig stop`, `brig rm` and `brig rm --all` remove each sandbox they stop from the broker." | broker.md: Lifecycle | `go:TestStopAndRemoveRevokeTheSandboxTokens` `go:TestRemoveAllRevokesEverySandboxsTokens` |
| "The line never holds a credential, a token, a header value or a body." | broker.md: Audit line | `go:TestAuditHoldsNoSecret` `go:TestPlaceholderAuditHoldsNoToken` `go:TestTheAuditLogHoldsNoValueOrToken` |
| "So the line of a request whose token has not verified carries only `method`, `status`, `dur`, `check` and `arrived`, and nothing the sender wrote into the path." | broker.md: Audit line | `go:TestAuditLineOfAnUnverifiedRequestCarriesNoPath` `go:TestAFloodOfUnverifiedRequestsIsBounded` `go:TestAnotherSandboxsPathReadsAsUnknownFromThisSocket` |
| "If the journal cannot be written, the refresh is not sent, Brig reports it as failing, and the broker tries again 30 seconds later at the earliest." | secrets.md: The renewal journal | `go:TestARefreshIsNotSentWithoutItsJournal` |
| "A refused access token in that window is not renewed again." | secrets.md: The renewal journal | `go:TestAFailedPromotionIsServedAndRetried` |
| "It also does this when a sandbox registers a route for the secret, without waiting for a request." | secrets.md: The renewal journal | `go:TestRegistrationRecoversAnInterruptedRefresh` |
| "Writes the journal's document to the secret, and deletes the journal. It does not call the provider" (a crash after the provider answered and the journal took the answer: `renewed`, with the secret still the document the refresh started from) | secrets.md: The renewal journal | `go:TestACrashAfterTheJournalPromotesIt` `go:TestRegistrationRecoversAnInterruptedRefresh` |
| "Keeps the secret, deletes the journal, and renews once at the next request, whatever the expiry." (a crash before the journal took the answer: `renewing`) | secrets.md: The renewal journal | `go:TestACrashBeforeAnyWriteRenewsOnceAndReportsSpent` `go:TestAJournalWhoseRefreshNeverLeftRenewsOnce` |
| "If the provider refuses the token as spent, the secret is reported as failing and is not renewed again." | secrets.md: The renewal journal | `go:TestACrashBeforeAnyWriteRenewsOnceAndReportsSpent` |
| "`renewed`, and the secret already holds the journal's document" (a crash after the secret took the document: the journal is deleted, and nothing else is written or called) | secrets.md: The renewal journal | `go:TestACrashAfterPromotionOnlyClearsTheJournal` |
| "Any journal, and the secret changed since the refresh started" (the journal is deleted, and the secret imported again stays) | secrets.md: The renewal journal | `go:TestARenewedJournalLosesToAReimport` |
| "A journal of another broker that still runs, younger than 10 minutes" (left alone, and not renewed) | secrets.md: The renewal journal | `go:TestAYoungJournalBlocksAndAStaleOneIsIgnored` `go:TestTwoCachesOverOneStoreAgree` |
| "A journal of another broker, older than 10 minutes or dated in the future" (treated as left by a broker that stopped) | secrets.md: The renewal journal | `go:TestAYoungJournalBlocksAndAStaleOneIsIgnored` `go:TestAJournalFromTheFutureIsStale` |
| "A journal of this broker that it did not know of" (deleted, and the renewal goes on) | secrets.md: The renewal journal | `go:TestAnOrphanJournalOfThisCacheIsDeleted` |
| "A journal Brig cannot read: damaged in the store, not one Brig wrote, or a `renewed` document that is not a credential of the secret's kind" (not promoted, and cleared) | secrets.md: The renewal journal | `go:TestAHalfWrittenJournalIsReportedAndCleared` `go:TestAJournalDocumentThatDoesNotParseIsNotPromoted` |
| "Serves the stored document, and reads the journal again 30 seconds later at the earliest" (the store does not answer) | secrets.md: The renewal journal | `go:TestAJournalThatCannotBeReadIsReported` |
| "A broker counts as running when a process of yours has that id and that start time." | secrets.md: The renewal journal | `go:TestAJournalOwnerIsThisUsersProcessWithItsStartTime` |
| "`brig secret delete` removes the journal with the secret." | secrets.md: The renewal journal | `go:TestDeleteAlsoRemovesTheRenewalJournal` `go:TestADeletedSecretTakesItsJournal` |
| "`brig secret read` and `delete` refuse a journal's own name, because a journal can hold the only valid copy of a renewed credential." | secrets.md: The renewal journal | `go:TestReadAndDeleteRefuseAJournalName` |
| "The broker keeps the keys in memory and writes none of them to the secret store or to a file." | The aws route | `go:TestNothingVendedOnDisk` |
| "The audit line names the access key id it handed out, and never the secret key or the session token." | The aws route | `go:TestVendAuditHasNoSecret` |
| "A run with a route that vends is refused when the profile or `BRIG_FORWARD_ENV` also hands the guest `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_PROFILE`, `AWS_DEFAULT_PROFILE`, `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`, `AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_ARN`, or an `AWS_CONTAINER_*` variable." | The aws route | `go:TestAVendingRunThatForwardsAWSVariablesIsRefused` |
| "It is also refused when `~/.aws` would be in the guest: a guest home that already holds `.aws/credentials` or `.aws/config` (for example `--home ~`), or a profile volume, `files:` binding, onboarding file or host config seed at `.aws`, a host config seed into the guest home itself, or a seed copied from your own `~/.aws`." | The aws route | `go:TestAVendingRunRefusesAGuestHomeWithAWS` `go:TestAVendingRunRefusesAWSPutInTheGuestHome` |
| "Brig compares `.aws` as a macOS file system can: with the case folded, and with a letter that is not ASCII taken for the ASCII letters it could fold into." | The aws route | `go:TestAVendingRunRefusesAWSPutInTheGuestHome` |
| "A run with a gcp route is refused when the profile or `BRIG_FORWARD_ENV` also hands the guest `GOOGLE_APPLICATION_CREDENTIALS`, `CLOUDSDK_CONFIG`, `CLOUDSDK_CORE_ACCOUNT`, `CLOUDSDK_ACTIVE_CONFIG_NAME`, a `CLOUDSDK_AUTH_*` variable or a `GCE_METADATA_*` variable." | The gcp route | `go:TestAGCPRunThatForwardsGoogleIdentityVariablesIsRefused` |
| "A profile that sets `HOME` to anything but the guest home is refused too, since `HOME` moves `.config/gcloud` as `CLOUDSDK_CONFIG` does." | The gcp route | `go:TestAGCPRunThatMovesHOMEIsRefused` |
| "gcloud creates both stores with no rows whenever it opens them, and an empty store is allowed; a store Brig cannot read as empty is refused." | The gcp route | `go:TestAGCPRunRefusesOnlyAGCloudStoreWithARow` `go:TestEmptySQLiteReadsPageHeaders` |
| "A gcloud configuration in `.config/gcloud/configurations`, or the legacy `.config/gcloud/properties`, that sets an `[auth]` property is refused, since `credential_file_override` or `impersonate_service_account` there points gcloud at an identity elsewhere; one Brig cannot read is refused too." | The gcp route | `go:TestAGCPRunRefusesAGCloudConfigurationThatSetsAuth` |
| "A tmpfs at `.config/gcloud` or `.config` is allowed: it carries nothing from the host." | The gcp route | `go:TestAGCPRunAllowsATmpfsOverGCloud` |
| "Brig compares `.config/gcloud` and `.config` as a macOS file system can: with the case folded, and with a letter that is not ASCII taken for the ASCII letters it could fold into." | The gcp route | `go:TestAGCPRunRefusesGCloudPutInTheGuestHome` |
| "The rest of `.config/gcloud`, which gcloud in the guest writes itself, is allowed." | The gcp route | `go:TestAGCPRunRefusesAGuestHomeWithAGCloudIdentity` `go:TestAGCPRunRefusesGCloudPutInTheGuestHome` |
| "A route that proxies does not send such a pointer upstream: it answers 502 with `check=credential.vendonly`, since the pointer names your AWS profile and role." | secrets.md: A secret behind a route | `go:TestAVendOnlySecretIsNotSentUpstream` `go:TestAWSPointerDocIsVendOnly` |
| "In role mode they last 30 minutes, or 25 minutes to an hour with `brig secret import --session-duration`." | The aws route | `go:TestPointerSessionBounds` `go:TestFetchAsksForThePointersSession` `go:TestImportAWSRefusesASessionOutOfRange` |
| "It refuses credentials with no expiry, such as the long-lived keys of an IAM user." | The aws route | `go:TestFetchRefusesNoExpiration` |
| "Self mode is never a default: `brig secret import` refuses an import that names neither mode, or both." | The aws route | `go:TestImportAWSNeedsAnAnswer` |
| "A secret you import again with another role or mode takes effect at the next fetch: the broker does not serve keys it fetched for the old secret, and fetches again." | The aws route | `go:TestCacheRefetchesOnPointerChange` |
| "The secret holds that pointer and no key." | The aws route | `go:TestImportAWSStoresNoCredential` |
| "The broker runs `aws` with every `AWS_` variable removed from its environment, so a variable in your shell, such as `AWS_PROFILE` or `AWS_ACCESS_KEY_ID`, never decides which identity a sandbox gets." | The aws route | `go:TestBrokerAWSEnvHasNoAWSVars` `go:TestFetchEnvIsExactlyCLIEnv` `go:TestEnvironDropsAWSVariables` `go:TestExecEmptyEnvIsEmpty` |
| "No package on the run path links the code that runs `aws`." | The aws route | `go:TestTheRunPathCannotReachAWS` `go:TestOnlyTheImportVerbReachesAWSFromTheCLI` |
| "The guest holds only the session token of the route." | The gcp route | `go:TestGuestEnvGCPRouteHoldsNoSecret` `go:TestServicesRouteForwardsToTheService` `go:TestServicesRouteGuestNeverSeesTheGoogleToken` |
| "A service that allows `POST` has to list paths that name their verb exactly, such as `/v2/entries:list`." | The gcp route | `go:TestServicesRouteVerbPathIsExact` `go:TestServicesRouteWildcardMatchesAVerb` `go:TestServicesRouteLoggingPostRead` |
| "The broker refuses the method-override headers `X-HTTP-Method-Override`, `X-HTTP-Method` and `X-Method-Override`, the query parameters `$httpMethod` and `httpMethod`, and batch requests." | The gcp route | `go:TestServicesRouteRefusesMethodOverride` `go:TestServicesRouteRefusesBatch` |
| "Self mode is never a default: `brig secret import` refuses a `gcloud` import that names neither mode, or both." | The gcp route | `go:TestGCloudImportNeedsAMode` `go:TestGCloudImportRefusesTwoModes` |
| "A token lives as long as `gcloud` makes it, about an hour, and the broker mints a new one when it has less than 2 minutes left." (the 2 minutes) | The gcp route | `go:TestGetMintsAgainUnderTheMargin` `go:TestAMintCloseToExpiryIsServedForTheFloor` |
| "A minted token is sent only on a route with `services`, to its Google APIs." | The gcp route | `go:TestAGCPRouteInjectsTheMintedToken` `go:TestAPointerOnARouteWithoutServicesIsRefused` |
| "A pointer on any other route is refused with `check=credential.gcponly`." | The gcp route | `go:TestAPointerOnARouteWithoutServicesIsRefused` |
| "A self-mode pointer also names the account of the login, and a token for any other account is refused." | The gcp route | `go:TestMintSelfRefusesAnotherAccount` `go:TestGCloudImportSelfNeedsAnAccount` |
| "In impersonate mode, a token that tokeninfo does not name as the service account is refused." | The gcp route | `go:TestMintRefusesAnotherIdentity` `go:TestMintImpersonateNeedsAnEmail` |
| "The secret holds that pointer and no token." | The gcp route | `go:TestGCloudImportStoresAPointerNotAToken` |
| "It runs `gcloud` without `GOOGLE_APPLICATION_CREDENTIALS`, `CLOUDSDK_CORE_ACCOUNT`, `CLOUDSDK_ACTIVE_CONFIG_NAME` and every `CLOUDSDK_AUTH_*` variable, so a variable in its environment does not decide which identity a sandbox gets." | The gcp route | `go:TestEnvironDropsIdentityVariables` `go:TestTheBrokersEnvironmentDoesNotPickTheIdentity` |
| "The import applies the same rule." | The gcp route | `go:TestDefaultGCloudCLIDropsIdentityVariables` |
| "Neither keeps `CLOUDSDK_CONFIG`, so both read the logins under `$HOME/.config/gcloud`, and the import refuses while `CLOUDSDK_CONFIG` is set." | The gcp route | `go:TestEnvironDropsIdentityVariables` `go:TestGCloudImportRefusesWhileCloudSDKConfigIsSet` |
| "No package on the run path links the code that runs `gcloud`." | The gcp route | `go:TestTheRunPathCannotReachGCloud` |
| "A secret you import again with another configuration, mode or service account takes effect at the next request." | The gcp route | `go:TestAPointerChangeIsNotServedTheOldToken` |
| "When `gcloud` can mint no token, a new run is refused with exit `6`, and a request from a running guest gets a 502." | The gcp route | `go:TestRegistrationIsRefusedWhenTheMintFails` `go:TestCredentialRefusalExits6` `go:TestAFailedMintSendsNothing` |
| "An upstream 401 to a minted token goes back to the guest, with no new mint and no retry." | The gcp route | `go:TestAMintedTokenPassesA401Through` `go:TestAMintedTokenIsNotRenewedOnARouteWithALifecycle` |
| "No token is on a command line, and no error repeats stdout." | broker.md: Minting a token | `go:TestMintArgvPerMode` `go:TestMintNeverEchoesStdout` |
| "So a new run on a lapsed gcloud login is refused at its start, and not at the agent's first Google call." | broker.md: Registration of a gcp route | `go:TestRegistrationIsRefusedWhenTheMintFails` `go:TestRegistrationMintsOnceForAPointer` |
| "A sandbox that was not registered before leaves no tokens behind." | broker.md: Registration of a gcp route | `go:TestRegistrationIsRefusedWhenTheMintFails` |
| "The broker serves only a route that vends on that path, and refuses any other route before it reads the token." | The aws route | `go:TestLinkLocalRefusesARouteThatDoesNotVend` `go:TestVendOnlyArrivalRefusesOtherRoutes` |
| "Brig adds no `allow` rule for it." (the link-local address of a route that vends) | Host services | `go:TestAVendingRunAllowsNoLinkLocalAddress` `go:TestRulesetDivertsTheLinkLocalAddress` |
| "A run without one diverts no link-local address." | broker.md: Transports | `go:TestARunWithoutAVendingRouteHasNoLinkLocalDivert` |
| "The broker closes a connection to any other port of the address." | broker.md: Transports | `go:TestLinkLocalOtherPortIsClosed` |
| "Every other route answers a token sent that way with 401." | broker.md: Routes that vend | `go:TestRawAuthorizationOnlyOnVendRoutes` |
| "The route serves its URL alone, with or without a trailing `/`." | broker.md: Routes that vend | `go:TestVendRefusesPostAndSubpaths` |
| "No key is on a command line, and no error repeats stdout." | broker.md: Fetching keys | `go:TestExecFakeAWS` `go:TestFetchErrorCarriesNoStdout` `go:TestFetchBadOutputCarriesNoOutput` `go:TestFetchTimesOut` |
| "A failed fetch is an error even when older keys are held, since those are about to expire." | broker.md: Fetching keys | `go:TestCacheNeverServesExpired` |
| "The error of the last row goes to `broker.log`, with the sandbox and the route, and never to the guest or to the audit log." | broker.md: Fetching keys | `go:TestAWSFailureGoesToTheBrokerLog` `go:TestVendErrorText` |
| "So a new run that cannot get AWS credentials is refused at its start, and not at the agent's first AWS call." | broker.md: Registration | `go:TestIdenticalRegistrationStillChecks` `go:TestServeRoutesMapsCredentialRefusal` `go:TestCredentialRefusalExits6` |
| "A sandbox that was not registered before leaves nothing behind." | broker.md: Registration | `go:TestRefusedRegistrationLeavesNoState` |
| "The client key never leaves the broker." | broker.md: Upstream | `go:TestTheClientKeyNeverReachesAuditLogOrBody` `macos-smoke:the client key never reaches the broker's audit or log` `vm:client-key-not-in-guest` |
| "A request on a route with `inject.tls` that fails a check before the dial does not reach the upstream." | broker.md: Upstream | `go:TestExpiredClientCertificateNeverDials` `go:TestNotYetValidClientCertificateNeverDials` `go:TestBadClientCertificateSecretIsTLSCertificate` `go:TestBadCASecretIsTLSCA` |
| "A certificate that cannot be read, that is not yet valid, or whose key does not match is `tls.certificate`, and a CA that cannot be read is `tls.ca`." | broker.md: Upstream | `go:TestBadClientCertificateSecretIsTLSCertificate` `go:TestNotYetValidClientCertificateNeverDials` `go:TestBadCASecretIsTLSCA` |
| "A handshake that fails is `tls.handshake`." | broker.md: Upstream | `go:TestUpstreamNotChainingToTheRouteCAIsRefused` `go:TestClientCertificateFromAnotherCAIsRefused` `go:TestHTTP2UpstreamRefusingTheClientCertificateIsAHandshakeFailure` `go:TestAResetIsAHandshakeFailureOnlyOnANewConnection` |
| "On a connection that served requests before, the handshake finished long ago, and a reset there is `upstream`." | broker.md: Upstream | `go:TestAResetIsAHandshakeFailureOnlyOnANewConnection` `go:TestRoundTripReportsANewConnectionOnlyOnce` |
| "every header in `inject.headers.remove` and `inject.headers.set`, with `Authorization` always among them" (a route with a client certificate and no credential) | broker.md: Headers | `go:TestCertificateOnlyRouteSendsNoSessionTokenUpstream` `macos-smoke:the upstream of a certificate-only route gets no Authorization and no session token` |
| "Two sandboxes never share a connection, even when they name the same route and the same upstream." | broker.md: Upstream | `go:TestTwoSandboxesNeverShareAClientCertificateConnection` |
| "It takes over no TLS session cache, key log or setting that skips verification, so no TLS session is shared across sandboxes or certificates." | broker.md: Upstream | `go:TestRouteTransportSharesNoTLSStateWithTheBase` |
| "On a route with `inject.tls.ca`, the `ca` certificates replace the system roots." | broker.md: Upstream | `go:TestRouteCAReplacesTheSystemRoots` `go:TestUpstreamNotChainingToTheRouteCAIsRefused` |
| "A route with `inject.tls.clientCertificate` and no `ca` still verifies the upstream with the system roots." | broker.md: Upstream | `go:TestRouteCAReplacesTheSystemRoots` |
| "The broker presents the certificate and signs the handshake with the key." | broker.md: Upstream | `go:TestClientCertificateIsPresentedUpstream` `vm:client-certificate-presented` |
| "Rotating the secret needs no restart of the sandbox or the broker." | broker.md: Upstream | `go:TestRotatedClientCertificateIsPresentedOnTheNextConnection` `go:TestRotationReachesTheNextRequestWhileAnHTTP2ConnectionIsBusy` `go:TestRotationOfTheCAReachesTheNextRequest` |
| "A request that was already in flight finishes on the connection it started on, with the old certificate, and that connection closes after it." | broker.md: Upstream | `go:TestRotationReachesTheNextRequestWhileAnHTTP2ConnectionIsBusy` |
| "A sandbox that is removed or replaced closes its connections the same way." | broker.md: Upstream | `go:TestRemovedSandboxDropsItsTransports` `go:TestRemovedSandboxBuildsNoTransportAfterwards` |
| "The line never holds the client key." | broker.md: Audit line | `go:TestTheClientKeyNeverReachesAuditLogOrBody` `macos-smoke:the client key never reaches the broker's audit or log` |
| "On success `create` and `update` print to stderr the certificate's subject, issuer and expiry, the length of the chain, and the subject of each certificate that will not be sent, and never the key." | secrets.md: Getting a value in | `go:TestSecretCreateTypeTLSNeverPrintsTheKey` `go:TestSecretCreateTypeTLSAcceptsEveryLayout` |
| "They print it after the value is stored, so a `create` that is refused because the name exists prints nothing." | secrets.md: Getting a value in | `go:TestSecretCreateTypeTLSPrintsTheSummaryOnlyAfterTheWrite` |
| "The next certificate is one that signed the one before and is a CA (basic constraints `CA:TRUE`), so a matching name is not enough." | secrets.md: Getting a value in | `go:TestParseClientCertificateTakesTheIntermediateThatSignedTheLeaf` `go:TestParseClientCertificatePrefersTheCrossSignedCopy` `go:TestParseClientCertificateNeverSendsANonCAWithTheIssuersName` `go:TestParseClientCertificateAcceptsEachKeyKind` |
| "the certificate's extended key usage does not allow client authentication" (a leaf whose extended key usage holds neither `clientAuth` nor `anyExtendedKeyUsage`) | secrets.md: Certificates and CA bundles | `go:TestParseClientCertificateRefusesEachStoreTimeCase` `go:TestParseClientCertificateAcceptsEachClientAuthUsage` |
| "A certificate that appears twice, byte for byte, counts once." | secrets.md: Getting a value in | `go:TestParseClientCertificateCountsTheSameCertificateOnce` `go:TestSecretCreateTypeTLSTakesTheSameCertificateOnce` `go:TestParseClientCertificateRefusesTwoCertificatesThatMatchTheKey` |
| "Any other block whose type ends in `PRIVATE KEY` is refused by name, not read as a missing key." | secrets.md: Getting a value in | `go:TestParseClientCertificateCountsOnlyThreeKeyBlockTypes` `go:TestParseClientCertificateSaysWhyInWords` |
| "The refusal reads `<verb> <name>: the value is not a client certificate: <reason>` for `tls`" | secrets.md: Getting a value in | `go:TestSecretCreateTypeTLSRefusesEachBadValue` `go:TestSecretCreateTypeTLSRefusesAnAmbiguousLayout` |
| "So the order of the blocks never matters, and a bundle that also carries the CA works as it is." | secrets.md: Getting a value in | `go:TestParseClientCertificateBuildsTheChainInAnyOrder` `go:TestSecretCreateTypeTLSAcceptsEveryLayout` |
| "The files are appended in the order given, with a line break between two when the first does not end in one, and the result is stored as given." | secrets.md: Getting a value in | `go:TestSecretCreateSeveralFilesAreAppendedInOrder` `go:TestSecretCreateTypeTLSAcceptsEveryLayout` |
| "The roots are then the certificates in that bundle that are CAs (basic constraints `CA:TRUE`), and the broker does not read the private key in it for that." | secrets.md: Getting a value in | `go:TestParseCAKeepsOnlyTheCertificatesThatAreCAs` `go:TestRouteCAMayNameTheClientCertificateSecret` |
