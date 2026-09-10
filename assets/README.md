# brig / hull brand marks

The wordmark is the mark. There is no symbol.

The letters are drawn on a square grid with a two-unit stroke, which is the
shape of a terminal cell and the shape of a bar. `brig` and `hull` share the
grid, the stroke and the palette, so the relationship between them is the
drawing itself rather than a symbol they both carry.

Every stroke is two units. The x-height is five units, the ascender is eight,
and the descender drops three below the baseline. Letters are tracked two units
apart.

## Palette

| Role | Hex | Use |
| --- | --- | --- |
| Navy | `#0E2233` | Field, and the ink on light backgrounds |
| Deep | `#071521` | The darker field, for terminal blocks |
| Paper | `#F2EFE6` | The ink on dark backgrounds |
| Brass | `#E7A33E` | The avatar glyph, and one accent per page |
| Brass deep | `#B9761C` | Brass on a light background, where the lighter brass fails contrast |

Brass because ships are brass; navy because it is the sea, not another
cloud-native blue. The files carry no font dependency, because there is no
font: the letters are rectangles.

## Files

| File | Use |
| --- | --- |
| `brig-lockup-on-{dark,light}.svg` | The wordmark, transparent, for README headers via `<picture>`. |
| `brig-lockup-badge.svg` | The wordmark on its own navy field, with clear space baked in, for slides and anywhere the background is uncontrolled. |
| `brig-avatar.svg`, `brig-avatar-480.png` | Org and repo avatar: the `b` on a full-bleed navy square, no baked corner radius, because GitHub rounds it. |
| `brig-mark-on-{dark,light}.svg` | The same glyph with the field taken away. |
| `nofire-logo.svg`, `nofire-logo-on-dark.svg` | The NOFire AI logo, for the README footer. |
| `architecture.svg` | The diagram in the README's "The boundary". README embeds it with `<img src>`, so the SVG's own internal `<title>` and `<desc>` are never read by a browser. The `img` element's `alt` attribute carries the description instead. Keep that alt text current whenever you change the diagram. |

The directory is flat. hull's own set of marks lives in
[brig-sh/brig-artwork](https://github.com/brig-sh/brig-artwork) with the
sources for these.

## Rules

- Lowercase, always. There is no capital form.
- Do not put the on-dark files on a light background. The ink is paper-coloured
  and disappears. That is what the on-light pair is for.
- Clear space is four units on every side. The badge already carries it; the
  transparent files do not.
- The wordmark holds down to about 33 px tall. Below that the stroke falls
  under two device pixels and the counters close up. Use the avatar instead.
- Scale by whole units. A fractional unit puts a stroke on a half pixel and the
  letters go soft.

These files are generated. Do not hand-edit them. Change the glyph table in
[brig-sh/brig-artwork](https://github.com/brig-sh/brig-artwork), rebuild, and
copy the result back here.
