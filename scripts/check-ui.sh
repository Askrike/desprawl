#!/usr/bin/env bash
# Renders every example's architecture and resources views in headless Chrome
# and checks the page draws exactly what the API returns, with no script
# errors. Complements `go test` (which checks the abstraction itself).
#
# usage: scripts/check-ui.sh
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
chrome="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
work=$(mktemp -d)
port=8097 static=8096
trap 'kill $server $web 2>/dev/null; wait $server $web 2>/dev/null || true; rm -rf "$work"' EXIT

cd "$root"
go build -o "$work/desprawl" ./cmd/desprawl
"$work/desprawl" -C examples serve -addr "localhost:$port" -open=false -layouts ../layouts >/dev/null 2>&1 &
server=$!
mkdir -p "$work/site/api"
python3 -I -m http.server "$static" -d "$work/site" >/dev/null 2>&1 &
web=$!
sleep 1

# The page is served statically with the API responses saved next to it and
# a probe appended that reports through document.title.
probe="<script>setTimeout(()=>{if(/^ERR/.test(document.title))return;
const arch=cy.nodes().not('[?isModule]').not('.prov').length+'/'+cy.edges().not('.prov').length;
level='raw';render();const raw=cy.nodes().not('[?isModule]').length;
level='arch';render();if(graph.arch.nodes.length)select(graph.arch.nodes[0].id);
document.title='OK '+arch+' '+raw},3000)</script></body>"
sed "s#<script>#<script>window.onerror=(m)=>{document.title='ERR '+m};#; s#</body>#${probe//$'\n'/}#" cmd/desprawl/index.html > "$work/site/index.html"

fail=0
for d in examples/*/; do
  name=$(basename "$d")
  curl -s "localhost:$port/api/graph?dir=$name" > "$work/site/api/graph"
  printf '{"root":"examples","current":"%s","dirs":["%s"]}' "$name" "$name" > "$work/site/api/dirs"
  want=$(jq -r '"\(.arch.nodes|length)/\(.arch.edges|length) \(.nodes|length)"' "$work/site/api/graph")
  got=$(perl -e 'alarm 40; exec @ARGV' "$chrome" --headless --disable-gpu --user-data-dir="$work/profile" \
    --virtual-time-budget=8000 --dump-dom "http://localhost:$static/" 2>/dev/null | grep -o '<title>[^<]*' | sed 's/<title>//') || true
  rm -rf "$work/profile"
  if [ "$got" = "OK $want" ]; then
    printf '%-34s ok (%s)\n' "$name" "$want"
  else
    printf '%-34s FAIL: want "OK %s", got "%s"\n' "$name" "$want" "$got"
    fail=1
  fi
done
exit $fail
