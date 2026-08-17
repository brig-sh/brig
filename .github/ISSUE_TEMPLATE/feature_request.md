---
name: Feature request / enhancement
about: Propose a new capability or an improvement to an existing one
title: ''
labels: enhancement
assignees: ''
---

<!--
Write a specific, imperative title, e.g. "Warn when a forwarded GH_TOKEN is a
classic PAT". Search open and closed issues first to avoid duplicates.
-->

## Problem

<!--
The problem or gap this addresses, and why it matters. If it builds on existing
code, ground it with a `file.go:line` reference or two so a reader can start
from the right place.
-->

## Proposed solution

<!-- What you would like to happen. -->

## Alternatives considered

<!-- Other approaches you weighed, and why this one. Delete if none. -->

## Effect on the sandbox boundary

<!--
Delete if there is none.

brig's value is that the guest sees one directory and holds only the
credentials it was named. A proposal that widens either -- a new mount, a new
forwarded variable, a new host path the guest can reach -- is still worth
making, but say so plainly here so the tradeoff is discussed rather than
discovered. docs/security.md is the place those limits are written down.
-->

## Additional context

<!-- Anything else that helps. Link related issues inline with #NN. -->
