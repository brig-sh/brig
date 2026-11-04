# Brig documentation

New to Brig? Read [the README](../README.md), then follow the
[Quickstart](quickstart.md).

## Tasks

| I want to | Read |
| --- | --- |
| Install Brig, or check a download | [Install](install.md) |
| Run a first agent on a throwaway project | [Quickstart](quickstart.md) |
| Run a second session, keep a guest home, or change the project | [Sessions, homes and projects](sessions.md) |
| Move my Claude Desktop or Claude Code sessions into a sandbox | [Moving Claude sessions](moving-claude-sessions.md) |
| Start an agent logged in | [Authentication](authentication.md) |
| Give the agent Git access over HTTPS | [Git access in the guest](authentication.md#git-access-in-the-guest) |
| Store a token or a certificate for an agent | [Secrets](secrets.md) |
| Limit what the sandbox can reach on the network | [Networking and egress policy](policies.md#quick-example) |
| Keep a login on the host, and give the guest a session token for it | [The credential broker](broker.md#use-it) |
| Open a port in the sandbox to the host | [`brig network`](cli.md#brig-network) |
| Run my own agent or my own image | [Writing an agent profile](profiles.md) and [Guest images](guest-image.md) |
| Run many sandboxes from one herdr client | [herdr](herdr.md) |
| Fix an error | [Troubleshooting](troubleshooting.md) |
| Turn on shell completion | [Completions](completions.md) |
| Turn off telemetry | [Telemetry](telemetry.md#turn-it-off) |
| Move off a retired command spelling | [Migration](migration.md) |

## Terms

| Term | Meaning |
| --- | --- |
| Agent | The tool Brig runs, such as Claude Code. |
| Profile | The file that says how to run an agent: its image, guest home and credentials. |
| Session, or ref | One selected session of an agent, written `<agent>[@<label>]`, such as `claude` or `claude@refactor`. |
| Sandbox | The microVM that runs a session. |
| Guest home | The host directory that the sandbox mounts as the agent's home. `brig ls`, `brig info` and `BRIG_WORKSPACE` call it the workspace. |
| Project | The host directory you name on the run line. The agent works on it at `/work/<name>`. |

## Understand

| Page | What it covers |
| --- | --- |
| [security.md](security.md) | What the sandbox isolates, and what an agent can still do |
| [claims.md](claims.md) | The tests behind each promise on the security page |
| [runtimes.md](runtimes.md) | The macOS backends, the Linux runtime, and how Brig chooses one |
| [non-goals.md](non-goals.md) | What Brig does not try to be |

## Reference

| Page | What it covers |
| --- | --- |
| [cli.md](cli.md) | Every verb and flag, the environment variables, the JSON output and the exit codes |
| [profiles.md](profiles.md) | Every profile field |
| [policies.md](policies.md) | Every policy field, route and preset, and where a policy is enforced |
| [broker.md](broker.md) | How the credential broker handles a request, and how it is hosted |
| [secrets.md](secrets.md) | The secret store, imports, renewal on the host |
| [guest-image.md](guest-image.md) | The contract an image must meet to boot as a guest |
| [brigd.md](brigd.md) | The session daemon and its protocol |
| [stability.md](stability.md) | What you can script against, and what can change |
| [telemetry.md](telemetry.md) | What Brig counts |
| [support.md](support.md) | Where to ask a question, file a bug or report a vulnerability |

## Contribute

| Page | What it covers |
| --- | --- |
| [../CONTRIBUTING.md](../CONTRIBUTING.md) | The build, the tests, the review norms |
| [../AI_POLICY.md](../AI_POLICY.md) | How AI-assisted contributions are handled |
| [../AGENTS.md](../AGENTS.md) | The contribution rules, for a coding agent that works in the repository |
| [../SECURITY.md](../SECURITY.md) | How to report a vulnerability |
| [releasing.md](releasing.md) | How to cut a release, for maintainers |
| [../assets/README.md](../assets/README.md) | The logo and diagram assets |

[manual-tests/](manual-tests/) holds dated engineering records. Each one
records what was measured on its date, and none is checked again against a
later release.
