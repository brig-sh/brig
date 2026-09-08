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
| `brig-avatar.svg`, `brig-avatar-512.png` | Org and repo avatar. Full-bleed navy square, no baked corner radius -- GitHub rounds it. The 512 PNG is the one GitHub takes for the org picture. |
| `brig-mark-on-{dark,light}.svg` | The mark alone, transparent background. |
| `brig-lockup-on-{dark,light}.svg` | Horizontal lockup, transparent, for README headers via `<picture>`. |
| `brig-lockup-badge.svg` | Lockup on its own navy field, for slides and anywhere the background is uncontrolled. |
| `nofire-logo.svg`, `nofire-logo-on-dark.svg` | The NOFire AI logo, for the README footer. |
| `architecture.svg` | The diagram in the README's "What brig is". |

The directory is flat. hull's own set of marks lives in
[brig-sh/brig-artwork](https://github.com/brig-sh/brig-artwork) with the
sources for these.

## Rules

- Do not put the on-dark mark on a light background. The ring is paper-coloured
  and disappears. That is what the on-light pair is for.
- Keep clear space of one ring width around the mark.
- The avatar is legible down to 16px; the rivets stop resolving at about 24px
  and that is fine, they are texture, not information.
- Do not re-colour the bars. Brass on navy is the only pairing that survives
  both GitHub themes.

Regenerate the PNG from the SVG with `rsvg-convert`.
