#!/bin/sh
# Usage (run only against an isolated hub with a test agent):
#   HUB=http://127.0.0.1:7760 TOKEN=<test-token> AGENT=<test-agent-id> N=30 \
#     sh tests/bench/dialog_bench.sh
# Measures N GETs to /api/sync/choices?direction=collect and prints P50/P95.
# All local state is confined to a fresh /tmp/bench-* directory; the hub and
# agent must be disposable too. This script never uses ~/.homer.
set -eu

: "${HUB:?set HUB to the isolated hub base URL}"
: "${TOKEN:?set TOKEN to the isolated hub bearer token}"
: "${AGENT:?set AGENT to the isolated agent ID}"
N=${N:-30}
case "$N" in
  ''|*[!0-9]*) echo "N must be a positive integer" >&2; exit 2 ;;
esac
if [ "$N" -lt 1 ]; then
  echo "N must be a positive integer" >&2
  exit 2
fi
command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 2; }

HUB=${HUB%/}
WORKDIR=$(mktemp -d /tmp/bench-XXXXXX)
trap 'rm -r "$WORKDIR"' EXIT HUP INT TERM
mkdir -p "$WORKDIR/home" "$WORKDIR/homer"
HOME=$WORKDIR/home
HOMER_HOME=$WORKDIR/homer
export HOME HOMER_HOME

AGENT_QUERY=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$AGENT")
TIMES=$WORKDIR/times.txt
BODY=$WORKDIR/response.json
i=1
while [ "$i" -le "$N" ]; do
  result=$(curl --silent --show-error --connect-timeout 3 --max-time 120 \
    --output "$BODY" --write-out '%{http_code} %{time_total}' \
    --header "Authorization: Bearer $TOKEN" \
    "$HUB/api/sync/choices?direction=collect&agent=$AGENT_QUERY") || {
      echo "request $i/$N failed; response: $(head -c 512 "$BODY" 2>/dev/null || true)" >&2
      exit 1
    }
  set -- $result
  status=$1
  elapsed=$2
  case "$status" in
    2*) ;;
    *)
      echo "request $i/$N returned HTTP $status; response: $(head -c 512 "$BODY" 2>/dev/null || true)" >&2
      exit 1
      ;;
  esac
  printf '%s\n' "$elapsed" >> "$TIMES"
  i=$((i + 1))
done

python3 - "$TIMES" "$N" <<'PY'
import math
import pathlib
import sys

values = sorted(float(line) for line in pathlib.Path(sys.argv[1]).read_text().splitlines())
count = int(sys.argv[2])

def percentile(p):
    return values[max(0, math.ceil(p * count) - 1)]

print(f"samples={count}")
print(f"P50={percentile(0.50):.6f}s")
print(f"P95={percentile(0.95):.6f}s")
PY
