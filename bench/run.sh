#!/usr/bin/env bash
set -euo pipefail

BENCH_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$BENCH_DIR")"
PORT=9876
UPSTREAM_PORT=9877
URL="http://127.0.0.1:${PORT}/"

# wrk parameters
DURATION=10
CONNECTIONS=256
THREADS=4
WARMUP=3           # warmup duration in seconds
RUNS=3

GREEN='\033[0;32m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

log()  { echo -e "${CYAN}[bench]${NC} $*"; }
ok()   { echo -e "${GREEN}[  ok ]${NC} $*"; }

kill_port() {
    local pids
    pids=$(lsof -ti:"$1" 2>/dev/null || true)
    [ -z "$pids" ] && return
    echo "$pids" | xargs kill 2>/dev/null || true
    sleep 0.5
    # Force kill any remaining
    pids=$(lsof -ti:"$1" 2>/dev/null || true)
    [ -z "$pids" ] && return
    echo "$pids" | xargs kill -9 2>/dev/null || true
    sleep 0.3
}

cleanup() {
    kill_port $PORT
    kill_port $UPSTREAM_PORT
    rm -f "$BENCH_DIR/prox-bin"
}
trap cleanup EXIT

wait_ready() {
    local port=$1 tries=0
    while ! curl -sf -o /dev/null "http://127.0.0.1:${port}/" 2>/dev/null; do
        tries=$((tries + 1))
        [ $tries -ge 50 ] && { echo "FAIL: port $port not ready"; return 1; }
        sleep 0.1
    done
}

run_single() {
    wrk -t$THREADS -c$CONNECTIONS -d${DURATION}s --latency "$URL" 2>&1
}

extract_rps()   { echo "$1" | grep "Requests/sec:" | awk '{printf "%.0f", $2}'; }
extract_avg()   { echo "$1" | grep "^[[:space:]]*Latency" | head -1 | awk '{print $2}'; }
extract_p99()   { echo "$1" | grep "99%" | awk '{print $2}'; }

bench_one() {
    local name=$1 best_rps=0 best_raw=""

    for run in $(seq 1 $RUNS); do
        # Warmup
        wrk -t2 -c64 -d${WARMUP}s "$URL" > /dev/null 2>&1 || true
        sleep 0.5

        local raw
        raw=$(run_single)
        local rps
        rps=$(extract_rps "$raw")

        if [ -n "$rps" ] && [ "$rps" -gt "$best_rps" ] 2>/dev/null; then
            best_rps=$rps
            best_raw="$raw"
        fi

        log "  Run $run: ${rps:-0} req/s"
    done

    local avg p99
    avg=$(extract_avg "$best_raw")
    p99=$(extract_p99 "$best_raw")
    NAMES+=("$name")
    RPS_LIST+=("$best_rps")
    AVG_LIST+=("${avg:-N/A}")
    P99_LIST+=("${p99:-N/A}")
}

NAMES=()
RPS_LIST=()
AVG_LIST=()
P99_LIST=()

echo ""
echo -e "${BOLD}═══════════════════════════════════════════════════════════════${NC}"
echo -e "${BOLD}  Reverse Proxy Benchmark${NC}"
echo -e "${BOLD}═══════════════════════════════════════════════════════════════${NC}"
echo ""
echo "  Machine:      $(sysctl -n machdep.cpu.brand_string)"
echo "  Cores:        $(sysctl -n hw.ncpu)"
echo "  RAM:          $(( $(sysctl -n hw.memsize) / 1024 / 1024 / 1024 )) GB"
echo "  wrk:          ${THREADS} threads, ${CONNECTIONS} connections, ${DURATION}s"
echo "  Runs:         ${RUNS} per proxy (best used)"
echo ""

# Start upstream
log "Starting upstream on :${UPSTREAM_PORT}..."
kill_port $UPSTREAM_PORT
(cd "$BENCH_DIR" && go run upstream.go &) 2>/dev/null
wait_ready $UPSTREAM_PORT
ok "Upstream ready"
echo ""

# ─── prox ─────────────────────────────────────────────────────────────────
log "Building prox..."
(cd "$PROJECT_DIR" && go build -o bench/prox-bin ./cmd/prox) 2>&1
log "Benchmarking ${BOLD}prox${NC}..."
kill_port $PORT
LOG_LEVEL=error GOMAXPROCS=3 "$BENCH_DIR/prox-bin" serve -config "$BENCH_DIR/prox.json5" &>/dev/null &
wait_ready $PORT
bench_one "prox"
kill_port $PORT
sleep 0.5
echo ""

# ─── nginx ────────────────────────────────────────────────────────────────
log "Benchmarking ${BOLD}nginx${NC}..."
kill_port $PORT
nginx -c "$BENCH_DIR/nginx.conf" 2>/dev/null
wait_ready $PORT
bench_one "nginx"
nginx -s quit 2>/dev/null || kill_port $PORT
sleep 0.5
echo ""

# ─── haproxy ──────────────────────────────────────────────────────────────
log "Benchmarking ${BOLD}haproxy${NC}..."
kill_port $PORT
haproxy -f "$BENCH_DIR/haproxy.cfg" -D 2>/dev/null
wait_ready $PORT
bench_one "haproxy"
kill_port $PORT
sleep 0.5
echo ""

# ─── caddy ────────────────────────────────────────────────────────────────
log "Benchmarking ${BOLD}caddy${NC}..."
kill_port $PORT
caddy run --config "$BENCH_DIR/Caddyfile" --adapter caddyfile &>/dev/null &
wait_ready $PORT
bench_one "caddy"
kill_port $PORT
sleep 0.5
echo ""

# ─── traefik ──────────────────────────────────────────────────────────────
log "Benchmarking ${BOLD}traefik${NC}..."
kill_port $PORT
traefik --configfile="$BENCH_DIR/traefik.yaml" &>/dev/null &
wait_ready $PORT
bench_one "traefik"
kill_port $PORT
sleep 0.5
echo ""

# ─── Results ──────────────────────────────────────────────────────────────
echo ""
echo -e "${BOLD}═══════════════════════════════════════════════════════════════${NC}"
echo -e "${BOLD}  Results (best of ${RUNS} runs)${NC}"
echo -e "${BOLD}═══════════════════════════════════════════════════════════════${NC}"
echo ""
printf "  ${BOLD}%-12s %10s %10s %10s${NC}\n" "Proxy" "Req/s" "Avg Lat" "P99 Lat"
printf "  %-12s %10s %10s %10s\n" "───────────" "─────────" "─────────" "─────────"
for i in "${!NAMES[@]}"; do
    printf "  %-12s %10s %10s %10s\n" "${NAMES[$i]}" "${RPS_LIST[$i]}" "${AVG_LIST[$i]}" "${P99_LIST[$i]}"
done
echo ""
echo "  Machine: $(sysctl -n machdep.cpu.brand_string), $(sysctl -n hw.ncpu) cores"
echo "  Test: wrk -t${THREADS} -c${CONNECTIONS} -d${DURATION}s, proxy → localhost:${UPSTREAM_PORT}"
echo ""
