# Move Claude sessions into Brig

You can carry Claude sessions into a Brig sandbox by copying files into the
guest home. This page covers the Claude Desktop Code tab, which runs in the
`claude-desktop` sandbox, and then the Claude Code CLI, which runs in
`claude-code`. Both are on macOS.

This was tried on macOS 26 with Brig 0.2.0 and hull 0.1.0-rc21, with one
session. The session appeared in the Code tab of the sandbox. Opening it
resumed it in `/work/<name>`, and the new message was appended to the copied
transcript with every earlier line unchanged. Brig's own tests do not cover
this. Try it on one session before you move the rest.

## Claude Desktop

### What a session is made of

The Desktop app keeps one session in two places on the host:

| What | Host path |
| --- | --- |
| The transcript | `~/.claude/projects/<project>/<id>.jsonl` |
| The entry in the session list | `~/Library/Application Support/Claude/claude-code-sessions/<account>/<org>/local_<id>.json` |

`<project>` is the working directory of the session with each `/` turned into
a dash, so `/Users/you/code/app` becomes `-Users-you-code-app`. The list entry
names the transcript in its `cliSessionId` field.

The sandbox keeps the same two things under the guest home, which you choose
with `--home`:

| What | Path under the guest home |
| --- | --- |
| The transcript | `.claude/projects/-work-<name>/<id>.jsonl` |
| The entry in the session list | `.config/Claude/claude-code-sessions/<account>/<org>/local_<id>.json` |

The project is mounted at `/work/<name>` in the guest, so that is the working
directory the guest sees. A session that still names the host path is not
listed against the project.

### Steps

The commands below copy and never move. Brig and this procedure read your host
files and write only under the guest home.

1. Pick the home and the project. The project directory becomes
   `/work/<name>`, where `<name>` is the directory's own name.

   ```bash
   HOME_DIR=~/brig-homes/desktop
   PROJECT=~/code/app            # mounted at /work/app
   mkdir -p "$HOME_DIR"
   ```

2. Copy the transcripts of that project, into the directory the guest names
   for `/work/app`.

   ```bash
   mkdir -p "$HOME_DIR/.claude/projects/-work-app"
   cp -p ~/.claude/projects/-Users-you-code-app/*.jsonl \
     "$HOME_DIR/.claude/projects/-work-app/"
   ```

3. Run once to create the app data and log in. The first run downloads the
   image and the boot assets, which is slow. See
   [What the first run costs](#what-the-first-run-costs).

   ```bash
   brig run claude-desktop "$PROJECT" --home "$HOME_DIR"
   ```

   Sign in to Claude inside the window. See [Logging in](#logging-in). The
   order matters only because that is the order that was tried. Nobody
   checked whether you can copy the session list entries before the first run.

4. Copy the session list entries whose `cliSessionId` matches a transcript you
   copied, and rewrite the working directory. The guest has to see the project
   at `/work/app`.

   ```bash
   python3 -I - "$HOME_DIR" <<'EOF'
   import glob, json, os, sys

   home = sys.argv[1]
   guest_cwd = "/work/app"
   host = os.path.expanduser(
       "~/Library/Application Support/Claude/claude-code-sessions")
   copied = {os.path.basename(p)[:-6] for p in glob.glob(
       os.path.join(home, ".claude/projects/-work-app/*.jsonl"))}

   for path in glob.glob(os.path.join(host, "*", "*", "local_*.json")):
       entry = json.load(open(path))
       if entry.get("cliSessionId") not in copied:
           continue
       account, org = path.split(os.sep)[-3:-1]
       out = os.path.join(home, ".config/Claude/claude-code-sessions",
                          account, org)
       os.makedirs(out, exist_ok=True)
       for key in ("cwd", "originCwd", "gitAnchorsFolderRealpath"):
           if key in entry:
               entry[key] = guest_cwd
       json.dump(entry, open(os.path.join(out, os.path.basename(path)), "w"))
       print("copied", os.path.basename(path))
   EOF
   ```

5. Quit the app in the sandbox and open it again. The Code tab lists the
   session. Open it and send a message. The transcript under
   `$HOME_DIR/.claude/projects/-work-app/` grows by the new lines, and the
   copy on your host does not change.

### Logging in

Claude Desktop in the sandbox asks you to sign in on the first run. The
sign-in opens a browser window inside the sandbox. After you finish signing
in, **close that browser window with its X**. The app does not show you as
signed in until the browser window is closed.

The login lives in the guest home, under `.config/Claude`, so it survives
`brig stop`. It is not the same login as `brig secret import claude-desktop`,
which fills a credential Brig forwards by name. See
[authentication.md](authentication.md).

### What the first run costs

On the machine this was tried on, the first `brig run claude-desktop` pulled
the image and grew `~/.hull` by about 3 GB, and took about 20 minutes. A later
run of the same image does not pull again.

## Claude Code CLI

The CLI keeps a session in the transcript alone. It has no session list entry,
so the Desktop steps 4 and 5 do not apply. The `claude-code` profile already
keeps `.claude/projects` on host disk, so you change no profile.

This was tried the same way: one session, macOS 26, Brig 0.2.0. In the
sandbox, `claude --resume <id>` found the copied session and appended to it,
every earlier line was unchanged, and the model answered from the history. The
interactive picker from a bare `claude --resume` was not tried, and the run used
the plain `claude-code` ref, not a label.

1. Copy the transcripts of the project into the guest home, as in
   [Claude Desktop step 2](#steps).

   ```bash
   HOME_DIR=~/brig-homes/cli
   mkdir -p "$HOME_DIR/.claude/projects/-work-app"
   cp -p ~/.claude/projects/-Users-you-code-app/*.jsonl \
     "$HOME_DIR/.claude/projects/-work-app/"
   ```

2. Give the guest your login, then run. Brig forwards the login at boot, so
   import it before the run.

   ```bash
   brig secret import claude-code
   brig run claude-code@moved ~/code/app --home "$HOME_DIR"
   ```

3. Inside the sandbox, the project is the working directory. Resume by id:

   ```bash
   claude --resume <id>
   ```

   The id is the file name of the transcript without `.jsonl`.

If the model call fails with `OAuth session expired and could not be
refreshed`, the login on your host has expired. Run `claude` on the host and
log in, run `brig secret import claude-code` again, and stop and run the
sandbox again. The failed call still appends lines to the copied transcript.

Use a label such as `claude-code@moved`. Running the plain `claude-code` ref
with a different `--home` moves an existing `claude-code` session onto that
home, and its record no longer points at the old one.

## Limits

- One run mounts one project, so one run covers the sessions of one project.
  Repeat steps 2 and 4 for each project, and mount each at its own
  `/work/<name>`.
- The `cwd` rewrite in step 4 touches three fields. The list entry has more
  fields that name host paths, for example `spawnSeed`. Nothing here shows
  whether those matter.
- Sessions made in the sandbox stay in the guest home. Nothing copies them
  back to the host.
- `claude-desktop` runs on macOS with the `vz` backend and a shared network
  only. See [install.md](install.md#claude-desktop).
