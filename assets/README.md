# brig / hull brand marks

The mark is a **porthole**. `brig` puts bars across it, because a brig is the
cell aboard a ship: the agent gets a window on the world and no way through it.
`hull` is the same porthole left open on water, because the hull is the vessel
the sandbox is carved out of.

Same silhouette, different contents. That is the whole system.

## Palette

| Role | Hex | Use |
| --- | --- | --- |
| Navy | `#0E2233` | Field, and the ink on light backgrounds |
| Deep | `#071521` | Porthole glass on dark |
| Paper | `#F2EFE6` | Ring and wordmark on dark |
| Brass | `#E7A33E` | Bars and water, on dark |
| Brass deep | `#B9761C` | Bars and water, on light (contrast) |

Brass because ships are brass; navy because it is the sea, not another
cloud-native blue. The wordmark is Avenir Next Demi Bold, converted to
outlines, so the files carry no font dependency.

## Files

| File | Use |
| --- | --- |
| `svg/brig-avatar.svg`, `png/brig-avatar-*.png` | Org and repo avatar. Full-bleed navy square, no baked corner radius -- GitHub rounds it. |
| `svg/brig-mark-on-{dark,light}.svg` | The mark alone, transparent background. |
| `svg/brig-lockup-on-{dark,light}.svg` | Horizontal lockup, transparent, for README headers via `<picture>`. |
| `svg/brig-lockup-badge.svg` | Lockup on its own navy field, for slides and anywhere the background is uncontrolled. |
| `svg/hull-*.svg` | The same set for `hull`. |

PNG exports live in `png/`. The 1024 and 512 avatars are what GitHub wants for
the org picture; everything else is convenience.

## Rules

- Do not put the on-dark mark on a light background. The ring is paper-coloured
  and disappears. That is what the on-light pair is for.
- Keep clear space of one ring width around the mark.
- The avatar is legible down to 16px; the rivets stop resolving at about 24px
  and that is fine, they are texture, not information.
- Do not re-colour the bars. Brass on navy is the only pairing that survives
  both GitHub themes.

Generated from `svg/`; regenerate PNGs with `rsvg-convert`.
