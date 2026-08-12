# Coming from the urunc-* Homebrew wrappers

brig replaces `urunc-claude`, `urunc-claude-desktop` and `urunc-ubuntu`. The
behaviour is the same, so this is mostly a matter of learning four verbs and
letting brig pick up the workspace you already have.

`urunc-macos` is not replaced. It is the runtime brig drives on macOS, and the
Homebrew cask depends on it.

## Commands

| was | now |
| --- | --- |
| `urunc-claude` | `brig run claude` |
| `urunc-claude --name foo` | `brig run claude --name foo` |
| `urunc-claude -p "..."` | `brig run claude -p "..."` |
| `urunc-claude --urunc-shell` | `brig shell claude` |
| `urunc-claude --urunc-shell 'cmd'` | `brig shell claude 'cmd'` |
| `urunc-claude --urunc-stop` | `brig stop claude` |
| `urunc-claude --urunc-env` | `brig env claude` |
| `urunc-claude-desktop` | `brig run claude-desktop` |
| `urunc-ubuntu` | `brig run ubuntu` |
| `urunc-ubuntu uname -a` | `brig run ubuntu uname -a` |

One difference worth noting: `--urunc-stop` also removed the instance, because
the wrapper had no other verb. `brig stop` only stops, and `brig rm` is the one
that takes the name back.

## Your workspace comes with you

An existing `~/sandboxed-claude`, `~/sandboxed-claude-desktop` or
`~/urunc-ubuntu` is adopted as the workspace when it is there, so history,
logins and checkouts carry over. A fresh install lands in `~/brig/<agent>`
instead.

Stop the old sandbox before the first brig run:

```bash
urunc-claude --urunc-stop
brig run claude
```

brig names its own microVM, so otherwise both are up with the same directory
shared into each, which is not something virtiofs enjoys.

## Settings

Every `URUNC_CLAUDE_*` variable is still read, so an existing shell profile
keeps working. The lookup order is per-agent, then global, then the legacy
name:

```
BRIG_CLAUDE_CODE_WORKSPACE  ->  BRIG_WORKSPACE  ->  URUNC_CLAUDE_WORKSPACE
```

Legacy names are read, never written. They map like this:

| wrapper | brig |
| --- | --- |
| `URUNC_CLAUDE_*` | `claude-code` |
| `URUNC_CLAUDE_DESKTOP_*` | `claude-desktop` |
| `URUNC_UBUNTU_*` | `ubuntu` |

The key names themselves are unchanged: `WORKSPACE`, `IMAGE`, `PULL`, `MEM`,
`CPUS`, `NAME`, `READY_TIMEOUT`, `FORWARD_ENV`, `CREDENTIALS_CMD`,
`ALLOW_REFS`, `GIT_CONFIG`, `GIT_HOSTS`, `GIT_USER`, `GIT_NAME`, `GIT_EMAIL`,
`GIT_IDENTITY`, `TRUST_WORKSPACE`, `TITLE`.

We would move to the `BRIG_*` spelling when convenient, but there is no hurry
and nothing breaks in the meantime.

## What is new

A few things the wrappers did not have:

- Guest images are verified before boot. See [security.md](security.md).
- Credentials no longer appear in `ps`. The wrapper documented this as a known
  limitation, and it is fixed.
- Agents other than Claude: `codex` is the second proven one, with `gemini`,
  `grok` and `opencode` as examples, plus your own via
  [templates](templates.md).
- `brig ls`, `brig rm` and `brig reset` for managing sandboxes, and `brig
  create` for starting one without attaching.

## What is not ported yet

The managed git files are named `.brig-git-credential` and `.gitconfig.brig`
rather than the `.urunc-*` spellings. An adopted workspace keeps the old files
too; they are harmless, and you can delete them once you are happy.

Workspace clone/overlay and explicit-apply are still open work. The workspace
is mounted directly, exactly as the wrapper mounted it.
