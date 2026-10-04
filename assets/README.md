# brig / hull brand marks

The mark is the wordmark. There is no symbol.

The letters are drawn on a square grid. `brig` and `hull` share the grid,
the stroke and the palette. That shared drawing relates the two marks. They
carry no common symbol.

| Measure | Units |
| --- | --- |
| Stroke | 2. It is the shape of a terminal cell and the shape of a bar. |
| x-height | 5 |
| Ascender | 8 |
| Descender | 3 below the baseline |
| Space between letters | 2 |

## Palette

| Role | Hex | Use |
| --- | --- | --- |
| Navy | `#0E2233` | Field, and the ink on light backgrounds |
| Deep | `#071521` | The darker field, for terminal blocks |
| Paper | `#F2EFE6` | The ink on dark backgrounds |
| Brass | `#E7A33E` | The avatar glyph, and one accent per page |
| Brass deep | `#B9761C` | Brass on a light background, where the lighter brass fails contrast |

Brass refers to ships. Navy refers to the sea. The files have no font
dependency, because the letters are rectangles.

## Files

| File | Use |
| --- | --- |
| `brig-lockup-on-{dark,light}.svg` | The wordmark, transparent, for README headers via `<picture>`. |
| `brig-lockup-badge.svg` | The wordmark on its own navy field, with clear space baked in, for slides and anywhere the background is uncontrolled. |
| `brig-avatar.svg`, `brig-avatar-480.png` | Org and repo avatar: the `b` on a full-bleed navy square, no baked corner radius, because GitHub rounds it. |
| `brig-mark-on-{dark,light}.svg` | The same glyph with the field taken away. |
| `nofire-logo.svg`, `nofire-logo-on-dark.svg` | The NOFire AI logo, for the README footer. |
| `brig-run-claude.gif` | The recording under the README's opening: Claude Code in a sandbox, writing a file into the project. It is a real session, recorded with [vhs](https://github.com/charmbracelet/vhs). Record it again when the output of `brig run` or of Claude Code changes enough to date it. |
| `architecture.svg` | The diagram in "Where the boundary sits" on the runtimes page. The page embeds it with `<img src>`, so a browser never reads the internal `<title>` and `<desc>` of the SVG. The `alt` attribute of the `img` element carries the description instead. When you change the diagram, update that alt text. |
| `brig-run-steps.svg`, `sandbox-mounts.svg`, `egress-policy.svg`, `secret-delivery.svg` | The diagrams on the quickstart, sessions, networking and secrets pages. Each carries its own styles and a dark variant, and draws its own background, so it reads on any page. The same alt-text rule applies. |

The directory is flat. The marks of hull, and the sources for these files,
are in [brig-sh/brig-artwork](https://github.com/brig-sh/brig-artwork).

## Rules

- Write the marks in lowercase. There is no capital form.
- Do not put the on-dark files on a light background. The ink is
  paper-coloured and disappears. Use the on-light pair there.
- Keep four units of clear space on every side. The badge already carries
  that space. The transparent files do not.
- Use the wordmark down to about 33 px tall. Below that height, the stroke
  is less than two device pixels and the counters close. Use the avatar
  there.
- Scale by whole units. A fractional unit puts a stroke on a half pixel and
  blurs the letters.
- Do not edit the marks by hand, because they are generated. Change the
  glyph table in
  [brig-sh/brig-artwork](https://github.com/brig-sh/brig-artwork), rebuild,
  and copy the result back here.
