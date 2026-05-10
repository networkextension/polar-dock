#!/usr/bin/env bash
# llm_bench.sh — load-test a Polar LLM call path via vegeta.
#
# Two modes:
#
#   ./scripts/llm_bench.sh assist <rps> <duration>
#       Hits POST /api/posts/assist-with-bot — stateless, returns a
#       short generated post body. Cheapest end-to-end path; doesn't
#       create chat threads / spawn agents. Best for "is the LLM
#       provider stable under N rps" questions.
#
#   ./scripts/llm_bench.sh chat <rps> <duration>
#       Hits POST /api/chat/messages with a fixed bot target. Same
#       upstream cost as assist but goes through aiAgent's full
#       runtime config resolution + project default LLM (#154/#155).
#       Best for "does the chat path scale" questions.
#
# Examples:
#   ./scripts/llm_bench.sh assist 2 30s
#   ./scripts/llm_bench.sh assist 5 1m
#   ./scripts/llm_bench.sh chat 1 30s
#
# Prereq:
#   1. brew install vegeta jq
#   2. ~/.polar/bench.env with the four lines below (do NOT commit):
#        POLAR_BASE_URL=https://zen.4950.store:2443
#        POLAR_ACCESS_TOKEN=<copy from browser cookie access_token after login>
#        POLAR_BOT_ID=<bot_user_id, e.g. bot_xxx>
#        POLAR_LLM_CONFIG_ID=<integer id from /api/llm-configs>
#
#   The token expires in 30min by default — re-grab from devtools
#   Application > Cookies if the script returns 401 right out of the
#   gate. Refresh-token flow is a browser thing; not worth scripting
#   for a one-off bench.
#
# Output:
#   /tmp/polar-bench-<mode>-<ts>.bin   raw vegeta results (replayable)
#   /tmp/polar-bench-<mode>-<ts>.json  parsed JSON report
#   stdout: vegeta human report (rate / latencies / status code dist)
#
# Companion: while a run is going, the LLM dashboard at
#   https://<host>/grafana/d/polar-llm shows polar_llm_request_total
#   and polar_llm_request_duration_seconds rate climb in real time.

set -euo pipefail

MODE="${1:-}"
RATE="${2:-}"
DURATION="${3:-30s}"

if [ -z "$MODE" ] || [ -z "$RATE" ]; then
  sed -n '2,/^$/p' "$0"
  exit 2
fi

ENV_FILE="${POLAR_BENCH_ENV:-$HOME/.polar/bench.env}"
if [ ! -f "$ENV_FILE" ]; then
  echo "missing $ENV_FILE — see header of $0 for the four required lines" >&2
  exit 1
fi
# shellcheck disable=SC1090
. "$ENV_FILE"

for v in POLAR_BASE_URL POLAR_ACCESS_TOKEN POLAR_BOT_ID POLAR_LLM_CONFIG_ID; do
  if [ -z "${!v:-}" ]; then
    echo "$v not set in $ENV_FILE" >&2
    exit 1
  fi
done

for c in vegeta jq curl; do
  if ! command -v "$c" >/dev/null 2>&1; then
    echo "missing command: $c (brew install $c)" >&2
    exit 1
  fi
done

TS=$(date +%Y%m%d-%H%M%S)
OUT_BIN="/tmp/polar-bench-${MODE}-${TS}.bin"
OUT_JSON="/tmp/polar-bench-${MODE}-${TS}.json"
TARGETS=$(mktemp -t polar-bench-targets)
BODY=$(mktemp -t polar-bench-body)
trap 'rm -f "$TARGETS" "$BODY"' EXIT

# Per-mode body + endpoint. Each prompt is short enough that the LLM
# cost is dominated by completion tokens (~few hundred), so tokens-per-
# minute scales linearly with rate — easy to reason about.
case "$MODE" in
  assist)
    URL="$POLAR_BASE_URL/api/posts/assist-with-bot"
    cat > "$BODY" <<JSON
{
  "bot_id": 0,
  "llm_config_id": $POLAR_LLM_CONFIG_ID,
  "topic": "polar load test ts=$TS",
  "instruction": "写一句不超过 30 字的开场白",
  "content": ""
}
JSON
    ;;
  chat)
    # Chat send needs a chat_id; pulling one fresh per-iteration is
    # painful. Operator should pre-create one and put it in env as
    # POLAR_CHAT_THREAD_ID. Falls back to /chats list if not set.
    if [ -z "${POLAR_CHAT_THREAD_ID:-}" ]; then
      echo "set POLAR_CHAT_THREAD_ID in $ENV_FILE — open /chat.html, copy the thread id from the URL" >&2
      exit 1
    fi
    URL="$POLAR_BASE_URL/api/chat/threads/$POLAR_CHAT_THREAD_ID/messages"
    cat > "$BODY" <<JSON
{"content": "polar bench probe ts=$TS — 用一句话回复 ok 即可"}
JSON
    ;;
  *)
    echo "unknown mode: $MODE (assist or chat)" >&2
    exit 2
    ;;
esac

cat > "$TARGETS" <<EOF
POST $URL
Cookie: access_token=$POLAR_ACCESS_TOKEN
Content-Type: application/json
@$BODY
EOF

echo "── Polar LLM bench ────────────────────────────────────"
echo "  mode:       $MODE"
echo "  url:        $URL"
echo "  rate:       $RATE/s"
echo "  duration:   $DURATION"
echo "  raw:        $OUT_BIN"
echo "  json:       $OUT_JSON"
echo "  watch live: $POLAR_BASE_URL/grafana/d/polar-llm"
echo "───────────────────────────────────────────────────────"
echo

vegeta attack -duration="$DURATION" -rate="$RATE" -targets="$TARGETS" \
  | tee "$OUT_BIN" \
  | vegeta report

vegeta report -type=json "$OUT_BIN" > "$OUT_JSON"

echo
echo "── Status code distribution ──"
vegeta report -type='hist[0,500ms,1s,2s,5s,10s,30s]' "$OUT_BIN"
echo
echo "── Top 5 slowest ──"
jq -r '.[] | "\(.code // 0) \(.latency / 1000000 | floor)ms"' \
  <(vegeta encode -to=json "$OUT_BIN") 2>/dev/null \
  | sort -k2 -n -r | head -5

echo
echo "Tips:"
echo "  - replay: vegeta plot $OUT_BIN > /tmp/plot.html && open /tmp/plot.html"
echo "  - rerun at higher rate to find the cliff (errors > 1%, p99 > 30s)"
echo "  - Grafana panels at /grafana/d/polar-llm should mirror these numbers"
