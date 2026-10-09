# Provider logos

The Lettered pair and Split pill can draw a small provider logo at the
top-right of each number (Settings → Menu bar → Badge). The logos are plain
SVG files bundled with the app:

| Provider ID in the summary | File |
| --- | --- |
| `claude` | `Resources/Logos/claude.svg` |
| `codex` | `Resources/Logos/codex.svg` |
| `xai` | `Resources/Logos/xai.svg` |

The files shipped in 0.4.0 are **placeholders** copied from the design board
(an eight-point spark, a hexagon ring and a slashed X). A provider without a
readable file gets no badge; the number keeps its letter.

## Swapping in the real logos

1. Replace the three files above, keeping their names. Each must be a
   single-colour SVG with a square `viewBox` (for example `0 0 16 16` or
   `0 0 24 24`). The small "glyph" or "symbol" version of a brand mark works
   best at 9pt.
2. Run the checks, which parse the three bundled files:

   ```sh
   cd plugins/quota-glance-menubar
   make test
   ```

3. Build (`make install` or `make dmg`). `scripts/build-app.sh` copies every
   `Resources/Logos/*.svg` into `Contents/Resources/Logos/`, and
   `scripts/verify-bundle.sh` fails if any of the three is missing.

No code changes are needed. To add a badge for another provider, add
`Resources/Logos/<providerID>.svg` using the summary's provider ID; IDs may
contain only letters, digits, `_` and `-`.

## What the files may contain

The app draws every painted shape in the readout's one ink: colours in the
file are ignored, `fill="none"` and zero opacity are honoured, so a file with
a coloured fill still draws as a solid mark. Supported:

- `path` (all commands, including arcs), `circle`, `ellipse`, `rect` (with
  `rx`/`ry`), `line`, `polyline`, `polygon`;
- nested `g` and `svg`, with `transform` (`matrix`, `translate`, `scale`,
  `rotate`, `skewX`, `skewY`);
- `fill`, `stroke`, `stroke-width`, `stroke-linecap`, `stroke-linejoin`,
  `fill-rule`, `opacity`, `fill-opacity`, `stroke-opacity`, `display`, and
  the same properties inside `style="…"`.

Skipped: `defs`, `clipPath`, `mask`, `symbol`/`use`, gradients, patterns,
filters, text and embedded images. Brand SVGs exported for print sometimes
use these; flatten them to plain paths first (for example, in a vector
editor, "Outline stroke" and "Flatten").

If a file cannot be read (no `viewBox` or `width`/`height`, invalid path
data, or no painted shapes), that provider simply has no badge, and
`make test` reports which file failed (`LogoMarkTests`).

## How they are drawn

Each logo is fitted into a 9 × 9pt square, 1pt after the number, its top 3pt
above the digits' cap height, as the board specifies. It is drawn as vectors
into the same template image as the letters and numbers, so it is crisp on
Retina displays and follows light and dark menu bars, the highlight and
Increase Contrast like the rest of the readout. The Split pill centres each
half's ink using the logo's own ink bounds, so a logo with empty space on
its right is not pushed off-centre.

The same files supply the 14pt logos in Settings' windows list and the 12pt
logos in each row's pop-up menu.

## Brand marks

Anthropic, OpenAI and xAI each publish rules for using their marks. Check
them before shipping a build with the real logos.
