#!/usr/bin/env bash
# Cost of the real agent binary on macOS: the first scan of your own transcripts (the last 30 days) and the idle state after it.
# Read-only on ~/.claude and the VS Code chat folders. The agent runs with a throwaway HARNEXT_HOME against a local dummy server
# that accepts every upload, so nothing leaves the machine and no real agent state is touched. It prints sizes, times and
# percentages, never paths or content.
#
#   make build && ./scripts/measure-agent.sh [bin/harnext-agent]     IDLE_SECONDS=60 by default
#
# A development build has no trusted keys, so it runs without friction analysis (the analysis cost is measured by
# TestMeasureScanAnalyzed in internal/watcher). Tested on macOS only; other systems are not measured.
set -euo pipefail

[ "$(uname -s)" = Darwin ] || { echo "macOS only: the sampling uses BSD ps" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 is needed for the dummy server" >&2; exit 1; }

BIN=${1:-bin/harnext-agent}
IDLE_SECONDS=${IDLE_SECONDS:-60}
[ -x "$BIN" ] || { echo "binary not found: $BIN (run make build)" >&2; exit 1; }
BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"

TMP=$(mktemp -d)
PIDS=()
cleanup() {
	for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	rm -rf "$TMP"
}
trap cleanup EXIT

cat >"$TMP/server.py" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        n = 0
        try:
            n = len(json.loads(body).get("sessions", []))
        except Exception:
            pass
        out = json.dumps({"accepted": n, "rejected": 0, "errors": []}).encode()
        self.send_response(200 if self.path == "/api/v1/ingest" else 404)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def do_GET(self):
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *a):
        pass

s = HTTPServer(("127.0.0.1", 0), H)
open(sys.argv[1], "w").write(str(s.server_address[1]))
s.serve_forever()
PY
python3 "$TMP/server.py" "$TMP/port" &
PIDS+=($!)
for _ in $(seq 1 50); do [ -s "$TMP/port" ] && break; sleep 0.1; done
PORT=$(cat "$TMP/port")

export HARNEXT_HOME="$TMP/home" HARNEXT_NO_AUTO_UPDATE=1
printf '%s\n' "hxt_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG" |
	"$BIN" init --token-stdin --server "http://127.0.0.1:$PORT" --dev-allow-insecure-localhost >/dev/null

# Input size, for the report: transcripts changed in the last 30 days.
INPUT=$(find "$HOME/.claude/projects" -name '*.jsonl' -mtime -30 -type f -exec stat -f '%z' {} + 2>/dev/null |
	awk '{n++; b+=$1} END {printf "%d transcripts, %.1f MB", n, b/1048576}')

# rss in KB and cumulative CPU seconds of a process: "rss cpu".
sample() {
	ps -o rss=,time= -p "$1" 2>/dev/null | awk '{
		n = split($2, t, ":"); s = 0
		for (i = 1; i <= n; i++) s = s * 60 + t[i]
		printf "%d %.2f\n", $1, s
	}'
}

# Memory the system charges to the process, in MB (macOS footprint). RSS also counts pages that Go freed and macOS has not
# taken back yet, so it reads several times higher; the footprint is the number to quote.
footprint_mb() {
	footprint "$1" 2>/dev/null | awk '{
		for (i = 1; i < NF; i++) if ($i == "Footprint:") {
			u = $(i + 2); v = $(i + 1)
			if (u == "KB") v /= 1024; else if (u == "GB") v *= 1024
			printf "%.0f", v; exit
		}
	}'
}

"$BIN" run >/dev/null 2>&1 &
AGENT=$!
PIDS+=("$AGENT")
START=$(python3 -c 'import time; print(time.time())')

peak=0
quiet=0
last_cpu=0
done_at=""
done_cpu=""
while kill -0 "$AGENT" 2>/dev/null; do
	read -r rss cpu < <(sample "$AGENT") || break
	[ "$rss" -gt "$peak" ] && peak=$rss
	delta=$(awk -v a="$cpu" -v b="$last_cpu" 'BEGIN {print a - b}')
	if awk -v d="$delta" 'BEGIN {exit !(d < 0.02)}'; then quiet=$((quiet + 1)); else quiet=0; fi
	last_cpu=$cpu
	if [ "$quiet" -ge 8 ]; then # four quiet seconds in a row
		now=$(python3 -c 'import time; print(time.time())')
		done_at=$(awk -v n="$now" -v s="$START" 'BEGIN {printf "%.1f", n - s - 4}')
		done_cpu=$cpu
		break
	fi
	sleep 0.5
done
[ -n "$done_at" ] || { echo "the agent stopped before it went quiet" >&2; exit 1; }
scan_footprint=$(footprint_mb "$AGENT")

idle_start_cpu=$last_cpu
idle_peak=0
end=$((SECONDS + IDLE_SECONDS))
while [ "$SECONDS" -lt "$end" ]; do
	read -r rss cpu < <(sample "$AGENT")
	[ "$rss" -gt "$idle_peak" ] && idle_peak=$rss
	last_cpu=$cpu
	sleep 1
done
read -r idle_rss _ < <(sample "$AGENT")
idle_footprint=$(footprint_mb "$AGENT")

echo "input:        $INPUT"
echo "cold start:   ~${done_at}s until quiet, ${done_cpu}s CPU, peak RSS $((peak / 1024)) MB, footprint ${scan_footprint:-n/a} MB after the scan"
awk -v a="$last_cpu" -v b="$idle_start_cpu" -v s="$IDLE_SECONDS" -v r="$idle_rss" -v p="$idle_peak" -v f="${idle_footprint:-n/a}" 'BEGIN {
	printf "idle (%ds):  %.3f%% of one core on average, footprint %s MB (RSS %d MB at the end, %d MB at most)\n", s, (a - b) / s * 100, f, r / 1024, p / 1024
}'
echo "agent folder: $(du -sk "$HARNEXT_HOME" | awk '{printf "%.1f MB", $1 / 1024}') on disk after the scan (local database, state, status)"
echo "system:       $(uname -sm), $(sysctl -n machdep.cpu.brand_string), build: $("$BIN" --version)"
