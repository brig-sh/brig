# Manual test: the keychain comment attribute

PR 2 (`internal/secret`) stores provenance in the keychain's `icmt` (comment)
attribute, and D9's design rests on two facts about `security(1)` that
`go test` cannot check without a real login keychain: that the comment can be
read back without a decrypt (so `brig secret ls` never raises an access
prompt), and that `-U` (update) actually replaces the comment rather than
leaving the old one in place. This is that probe, run by hand against a
throwaway item under `sh.brig.probe` -- never `sh.brig.secret`, so it cannot
collide with anything the real store owns -- on the real login keychain,
because there is no other keychain `security -i`'s `-w`-must-be-last rule
would exercise the same way (see `keychain_darwin_test.go`'s own comment on
why its tests use the real keychain).

CI cannot run this: `ci.yml` is Linux-only and GitHub Actions is disabled at
the repository level, so none of it is machine-verified.

## What was run

Exactly the plan's step 1 transcript, on macOS 26.4 (Darwin 25.4.0, arm64).

```
$ security add-generic-password -s sh.brig.probe -a p1 -l "brig: p1" -D "brig secret" \
    -j "brig1:eyJ2IjoxfQ" -w cHJvYmU=
(exit 0, no prompt)

$ security dump-keychain | grep -A2 'sh.brig.probe'
```

The item's full block in the dump (found by `grep -n` for the service name
rather than trusting a fixed `-A` window, since the attribute lines are not in
a fixed order):

```
keychain: "/Users/alexandros/Library/Keychains/login.keychain-db"
version: 512
class: "genp"
attributes:
    0x00000007 <blob>="brig: p1"
    0x00000008 <blob>=<NULL>
    "acct"<blob>="p1"
    "cdat"<timedate>=0x32303236303831373136303731335A00  "20260817160713Z\000"
    "crtr"<uint32>=<NULL>
    "cusi"<sint32>=<NULL>
    "desc"<blob>="brig secret"
    "gena"<blob>=<NULL>
    "icmt"<blob>="brig1:eyJ2IjoxfQ"
    "invi"<sint32>=<NULL>
    "mdat"<timedate>=0x32303236303831373136303731335A00  "20260817160713Z\000"
    "nega"<sint32>=<NULL>
    "prot"<blob>=<NULL>
    "scrp"<sint32>=<NULL>
    "svce"<blob>="sh.brig.probe"
    "type"<uint32>=<NULL>
```

**Fact 1 confirmed:** `"icmt"<blob>="brig1:eyJ2IjoxfQ"` appears in the dump
verbatim, with no `-d` and no keychain-access prompt of any kind. Attributes
only, exactly as `List`/`parseDump` already read `svce`, `acct` and `mdat`.

```
$ security add-generic-password -s sh.brig.probe -a p1 -l "brig: p1" -D "brig secret" \
    -j "brig1:eyJ2IjoyfQ" -U -w cHJvYmU=
(exit 0, no prompt)

$ security dump-keychain | grep -A2 'sh.brig.probe'
```

The same block, re-dumped:

```
    "icmt"<blob>="brig1:eyJ2IjoyfQ"
    ...
    "mdat"<timedate>=0x32303236303831373136303732385A00  "20260817160728Z\000"
```

**Fact 2 confirmed:** the comment changed from `brig1:eyJ2IjoxfQ` (`{"v":1}`)
to `brig1:eyJ2IjoyfQ` (`{"v":2}`), and `mdat` advanced from `160713Z` to
`160728Z` -- `-U` replaced the comment along with the value, it did not leave
the original `icmt` behind.

```
$ security delete-generic-password -s sh.brig.probe -a p1
password has been deleted.

$ security dump-keychain | grep -c 'sh.brig.probe'
0

$ security find-generic-password -s sh.brig.probe -a p1
security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.
(exit 44)
```

Cleanup confirmed: nothing under `sh.brig.probe` is left in the keychain.

## Conclusion

Both facts D9 needs hold, verbatim, on this host: `dump-keychain` prints
`icmt` without `-d` and without a decrypt prompt, and `-U` carries a new
comment through an update rather than preserving the old one. The rest of
this PR (`Write`, `MaxValueFor`, `parseDump` reading `icmt` through
`DecodeProvenance`) proceeds as designed.
