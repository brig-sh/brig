# brig documentation

Start with [the README](../README.md) for what brig is and a first run. This
index is the map of everything else.

## Get started

| Page | What it gives you |
| --- | --- |
| [install.md](install.md) | Every way to install brig, per platform, and how to verify a download |
| [quickstart.md](quickstart.md) | One agent running on a throwaway project, start to finish |
| [support.md](support.md) | Where to ask a question or file a bug |

## Use brig

| Page | What it gives you |
| --- | --- |
| [sessions.md](sessions.md) | Guest homes, projects, named sessions, and what survives which command |
| [authentication.md](authentication.md) | Getting an agent logged in, and Git access inside the guest |
| [secrets.md](secrets.md) | The secret store, profile secret fields, and `brig secret import` |
| [policies.md](policies.md) | Networking modes and egress policy |
| [profiles.md](profiles.md) | The profile file format, and running your own agent |
| [guest-image.md](guest-image.md) | The contract an image must meet to boot as a guest |
| [completions.md](completions.md) | Shell completion for bash, zsh and fish |

## Look something up

| Page | What it gives you |
| --- | --- |
| [cli.md](cli.md) | Every verb and flag, the environment variables, the JSON output and the exit codes |
| [migration.md](migration.md) | Every retired spelling and its replacement |
| [stability.md](stability.md) | What you can script against, and what is expected to move |
| [telemetry.md](telemetry.md) | What brig counts, and how to turn it off |

## Understand the design

| Page | What it gives you |
| --- | --- |
| [security.md](security.md) | What the sandbox isolates, and what an agent can still do |
| [non-goals.md](non-goals.md) | What brig does not try to be |
| [runtimes.md](runtimes.md) | The macOS backends, the Linux runtimes, and how one is chosen |
| [brigd.md](brigd.md) | What the daemon is for |

## Fix something

| Page | What it gives you |
| --- | --- |
| [troubleshooting.md](troubleshooting.md) | Organized by the error you saw, with the command that confirms the fix |

## Contribute

| Page | What it gives you |
| --- | --- |
| [../CONTRIBUTING.md](../CONTRIBUTING.md) | The build, the tests, the review norms |
| [../AI_POLICY.md](../AI_POLICY.md) | How AI-assisted contributions are handled |
| [../SECURITY.md](../SECURITY.md) | Reporting a vulnerability |
| [releasing.md](releasing.md) | Cutting a release. Maintainers only |
| [../assets/README.md](../assets/README.md) | The logo and diagram assets, and how to use them |

## Historical records

[manual-tests/](manual-tests/) holds dated engineering records from specific
changes. They evidence what was measured on the day they were written. They are
not current validation, and each one says so in its own header.

| Record | What it measured |
| --- | --- |
| [manual-tests/egress-policy.md](manual-tests/egress-policy.md) | Egress rules against a live gateway |
| [manual-tests/runtime-stdin.md](manual-tests/runtime-stdin.md) | Standard input through the runtime |
| [manual-tests/sandbox-reachability.md](manual-tests/sandbox-reachability.md) | What a guest could reach from inside the sandbox |
| [manual-tests/secret-files.md](manual-tests/secret-files.md) | File-delivered secrets and their tmpfs backing |
| [manual-tests/secret-provenance.md](manual-tests/secret-provenance.md) | Where an imported secret came from |
