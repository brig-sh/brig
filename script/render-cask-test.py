#!/usr/bin/env python3
"""Checks that script/render-cask.py writes a brig channel cask Homebrew 7 accepts.

Homebrew 7 loads every cask in a tap on each OS and arch it can simulate,
and refuses the whole tap when one of them fails. check() repeats its two
cask checks on the renderer's output:

- on macOS, each arch needs a url, unless the cask depends on Linux. This
  check ignores `depends_on arch:`.
- on Linux, each arch needs a sha256, unless the cask depends on macOS or
  its `depends_on arch:` leaves that arch out.

It reads only the Ruby the renderer writes. `brew readall` and `brew audit`
stay the authority; this runs without Homebrew, on any host.

Usage: python3 script/render-cask-test.py
"""

import pathlib
import re
import subprocess
import sys
import tempfile
import unittest

RENDER = pathlib.Path(__file__).resolve().parent / "render-cask.py"
VERSION = "0.2.1-0.20260925182627-4ce5eeecd8ac"
TAG = f"channel-main-{VERSION}"
PLATFORMS = [(os, arch) for os in ("macos", "linux") for arch in ("arm", "intel")]
GOARCH = {"arm": "arm64", "intel": "amd64"}
GOOS = {"macos": "darwin", "linux": "linux"}


def render():
    """Runs the renderer as channel.yml does, on empty archives, and returns the cask."""
    with tempfile.TemporaryDirectory() as tmp:
        dist = pathlib.Path(tmp)
        archives = []
        for os, arch in PLATFORMS:
            name = f"brig-{VERSION}-{GOOS[os]}-{GOARCH[arch]}.tar.gz"
            (dist / name).write_bytes(b"")
            archives.append(f"--archive=on_{os}:on_{arch}:{name}")
        out = dist / "cask.rb"
        subprocess.run([
            sys.executable, str(RENDER),
            "--project", "brig", "--channel", "main", "--version", VERSION,
            "--repo", "brig-sh/brig", "--tag", TAG, "--dist", str(dist),
            *archives,
            "--desc", "Run a coding agent in a microVM sandbox",
            "--source", "the tip of main",
            "--binary=brig", "--binary=brigd",
            "--completion=bash", "--completion=zsh", "--completion=fish",
            "--depends-cask=brig-sh/brig/hull@main", "--depends-formula=cosign",
            "--conflicts-with=experimental",
            "--out", str(out),
        ], check=True, capture_output=True)
        return out.read_text()


def evaluate(cask, os, arch):
    """Returns the url, sha256, OS and arch dependency the cask gives one platform."""
    found = {"url": None, "sha256": None, "os": None, "arch": None}
    blocks = []
    heredoc = False
    for line in cask.splitlines():
        s = line.strip()
        if heredoc:
            heredoc = s != "EOS"
            continue
        if s.endswith("<<~EOS"):
            heredoc = True
            continue
        block = re.fullmatch(r"(\S+).* do", s)
        if block:
            name = block.group(1)
            blocks.append(not name.startswith("on_") or name in (f"on_{os}", f"on_{arch}"))
            continue
        if s == "end":
            blocks.pop()
            continue
        if not all(blocks):
            continue
        m = re.fullmatch(r'(url|sha256) "(.*)"', s)
        if m:
            found[m.group(1)] = m.group(2)
        m = re.fullmatch(r"depends_on :(macos|linux)", s)
        if m:
            found["os"] = m.group(1)
        m = re.match(r"depends_on arch: +:(\w+)", s)
        if m:
            found["arch"] = m.group(1)
    return found


def check(cask):
    """Returns what Homebrew would refuse, and the platforms the cask installs on."""
    refused, installs = [], set()
    for os, arch in PLATFORMS:
        on = evaluate(cask, os, arch)
        if on["os"] not in (None, os):
            continue
        if os == "macos" and not on["url"]:
            refused.append(f"macOS on {arch}: no url")
        if on["arch"] not in (None, {"arm": "arm64", "intel": "x86_64"}[arch]):
            continue
        if os == "linux" and not on["sha256"]:
            refused.append(f"Linux on {arch}: no sha256")
        installs.add((os, arch))
    return refused, installs


class RenderCaskTest(unittest.TestCase):
    def test_every_platform(self):
        cask = render()
        refused, installs = check(cask)
        self.assertEqual(refused, [], cask)
        self.assertEqual(installs, set(PLATFORMS), cask)

    def test_url_names_the_version(self):
        # brew audit takes a url without #{version} for an unversioned one.
        cask = render()
        for os, arch in PLATFORMS:
            self.assertEqual(
                evaluate(cask, os, arch)["url"],
                "https://github.com/brig-sh/brig/releases/download/channel-main-#{version}/"
                f"brig-#{{version}}-{GOOS[os]}-{GOARCH[arch]}.tar.gz")


if __name__ == "__main__":
    unittest.main()
