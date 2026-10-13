#!/usr/bin/env bash
# Rebuild the pixel captures embedded in refined.html, and optionally the
# board screenshots.
#   ./capture.sh          embed fresh 1x and 2x sprites into refined.html
#   ./capture.sh shots    the same, then write shots/r2-*.png
# Run it after replacing the logo <symbol>s in refined.html, or after changing
# anything the captures show: they are embedded PNGs and do not follow edits.
# Needs agent-browser (Chrome) and python3 (standard library only).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SESSION="${SESSION:-r2-menu-revise}"
# Greyscale antialiasing and no hinting, as macOS draws text. Launch flags
# only apply when the browser starts, so the session is closed first.
ARGS="--disable-lcd-text,--font-render-hinting=none"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
ab(){ agent-browser --session "$SESSION" "$@"; }
ab close >/dev/null 2>&1 || true
# The daemon may still be shutting down after close: retry the launch briefly.
for try in 1 2 3 4 5; do
  agent-browser --session "$SESSION" --args "$ARGS" open about:blank >/dev/null 2>&1 && break
  [ "$try" = 5 ] && { echo "could not launch the browser" >&2; exit 1; }
  sleep 1
done
BOARD="file://$DIR/refined.html"
ready(){ ab wait "html[data-ready='1']" >/dev/null; ab wait 400 >/dev/null; }

# 1. Sprites: lay every captured bar out on one sheet, screenshot it at device
#    scale 1 and 2, check the layout matched, and embed both PNGs.
ab set viewport 1600 900 1 >/dev/null
ab open "$BOARD?capture=1" >/dev/null; ready
ab eval "JSON.stringify(window.__layout)" > "$TMP/layout0.json"
read -r W H < <(python3 -I -c "import json,sys;d=json.loads(json.loads(open(sys.argv[1]).read()));print(d['W'],d['H'])" "$TMP/layout0.json")
for dpr in 1 2; do
  ab set viewport "$W" "$H" "$dpr" >/dev/null
  ab open "$BOARD?capture=1" >/dev/null; ready
  ab eval "JSON.stringify(window.__layout)" > "$TMP/layout$dpr.json"
  ab screenshot "$TMP/sprite$dpr.png" >/dev/null
done
cmp -s "$TMP/layout1.json" "$TMP/layout2.json" || { echo "layout differs between device scale 1 and 2" >&2; exit 1; }
python3 -I - "$TMP" "$DIR/refined.html" <<'PY'
import base64, json, re, struct, sys
tmp, html = sys.argv[1], sys.argv[2]
layout = json.loads(json.loads(open(f'{tmp}/layout1.json').read()))
def dims(p):
    b = open(p, 'rb').read(24)
    return struct.unpack('>II', b[16:24])
for d in (1, 2):
    w, h = dims(f'{tmp}/sprite{d}.png')
    assert (w, h) == (layout['W'] * d, layout['H'] * d), (d, w, h, layout['W'], layout['H'])
uri = lambda p: 'data:image/png;base64,' + base64.b64encode(open(p, 'rb').read()).decode()
blob = 'const SPRITES = ' + json.dumps({'layout': layout, 'x1': uri(f'{tmp}/sprite1.png'), 'x2': uri(f'{tmp}/sprite2.png')}, separators=(',', ':')) + ';'
s = open(html).read()
s2, n = re.subn(r'/\*SPRITES:BEGIN\*/.*?/\*SPRITES:END\*/', lambda m: '/*SPRITES:BEGIN*/' + blob + '/*SPRITES:END*/', s, flags=re.S)
assert n == 1
open(html, 'w').write(s2)
print('embedded sprites', layout['W'], 'x', layout['H'], 'logo hash', layout['sym'])
PY
[ -n "${KEEP_SPRITES:-}" ] && cp "$TMP"/sprite*.png "$KEEP_SPRITES"/

# 2. Screenshots (./capture.sh shots). Section shots are 2x; a short viewport
#    keeps each shot to its content's height.
[ "${1:-}" = shots ] || exit 0
S="$DIR/shots"
mkdir -p "$S"
shot(){ # file query width height dpr
  ab set viewport "$3" "$4" "$5" >/dev/null
  ab open "$BOARD$2" >/dev/null; ready
  ab screenshot --full "$S/$1" >/dev/null
  echo "wrote shots/$1"
}
shot r2-board.png          ""                         1600 900 1
shot r2-overview@2x.png    "?only=head"               1600 300 2
shot r2-rules.png          "?only=rules"              1600 300 2
shot r2-badge-sizes.png    "?only=badge"              1600 300 2
for c in sw sf fw codexw-claudew xaic-claudew three; do
  shot "r2-combo-$c.png"   "?only=combo-$c"           1600 300 2
done
shot r2-modes.png          "?only=modes"              1600 300 2
shot r2-menubars.png       "?only=bars"               1600 300 2
shot r2-settings.png       "?only=settings"           1600 300 2
shot r2-build.png          "?only=questions&open=1"   1600 300 2
# One element only: the "Split pill at 100" showcase and the header comparison.
shotel(){ # file query selector width height dpr
  ab set viewport "$4" "$5" "$6" >/dev/null
  ab open "$BOARD$2" >/dev/null; ready
  ab screenshot "$3" "$S/$1" >/dev/null
  echo "wrote shots/$1"
}
shotel r2-pill-100.png     "?only=p100"     "#s-p100"  1600 2000 2
shotel r2-pill-compare.png "?only=compare"  "#compare" 1600 1200 2
