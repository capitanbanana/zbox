#!/usr/bin/env bash
# Real VM smoke test; downloads an Ubuntu image and packages. No model calls.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
binary="$repo/bin/zbox"
testroot=$(mktemp -d /tmp/zbox-test.XXXXXX)
state="${ZBOX_SMOKE_STATE:-$testroot/state}"
exports="${ZBOX_SMOKE_EXPORTS:-$testroot/exports}"
host_pid=''
zbox() { "$binary" --state-dir "$state" --vm smoke "$@"; }
cleanup() {
  if [[ -n "$host_pid" ]]; then kill "$host_pid" 2>/dev/null || true; fi
  if [[ -f "$state/vms/smoke/config.json" ]]; then
    if ! zbox delete; then
      printf 'Cleanup failed; state retained at %s\n' "$testroot" >&2
      return
    fi
  fi
  rm -rf "$testroot"
}
trap cleanup EXIT
mkdir -p "$exports/rw" "$exports/ro" "$exports/new"
printf 'host input\n' > "$exports/ro/input"
if [[ ! -f "$state/vms/smoke/config.json" ]]; then
  zbox create --cpus 2 --memory 4 --disk 20
fi
zbox mounts add "$exports/rw"
zbox mounts add "$exports/ro" --ro
zbox start
zbox shell sh -c 'test -z "${SSH_AUTH_SOCK:-}"; test ! -e /Users; command -v claude; command -v codex; command -v pi; command -v ori'
zbox claude "$exports/rw" --version
zbox codex "$exports/rw" --version
zbox pi "$exports/rw" --version
zbox ori "$exports/rw" --version
zbox openrouter "$exports/rw" --version
zbox shell sh -c 'printf "guest output\n" > /home/agent/dev/rw/result; cat /home/agent/dev/ro/input; ! touch /home/agent/dev/ro/forbidden'
test "$(cat "$exports/rw/result")" = 'guest output'
test ! -e "$exports/ro/forbidden"
if zbox mounts add "$exports/new"; then
  printf 'ERROR: silently restarted VM for new mount\n' >&2; exit 1
fi
zbox shell curl --fail --max-time 30 -sS https://example.com -o /dev/null
zbox network add example.com '*.selectel.ru'
zbox shell curl --fail --max-time 30 -sS https://example.com -o /dev/null
grep -q 'HOST "example.com:443"' "$state/vms/smoke/proxy.log"

# An explicitly granted host MCP port; the stand-in serves a health marker.
python3 - "$testroot/host-port" <<'PY' &
import http.server,sys
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b'host-mcp-ok')
    def log_message(self,*args): pass
s=http.server.HTTPServer(('127.0.0.1',0),Handler)
with open(sys.argv[1],'w') as f: f.write(str(s.server_port))
s.serve_forever()
PY
host_pid=$!
for _ in {1..50}; do [[ -s "$testroot/host-port" ]] && break; sleep .1; done
host_port=$(cat "$testroot/host-port")
zbox mcp add smoke-host --url "http://127.0.0.1:$host_port/mcp" --host
sleep 3
test "$(zbox shell curl --fail --max-time 5 -sS --noproxy '*' http://127.0.0.1:19000/mcp)" = host-mcp-ok
zbox codex "$exports/rw" mcp list --json > "$testroot/mcp.json"
python3 - "$testroot/mcp.json" <<'PY'
import json,sys
rows=json.load(open(sys.argv[1]))
assert any(r['name']=='smoke-host' and r['enabled'] for r in rows), rows
PY
zbox mcp disable smoke-host
sleep 3
if zbox shell curl --fail --max-time 3 -sS --noproxy '*' http://127.0.0.1:19000/mcp; then
  printf 'ERROR: disabled host MCP tunnel still reachable\n' >&2; exit 1
fi
zbox stop
zbox mounts add "$exports/new"
zbox start
zbox shell test -d /home/agent/dev/new
zbox status
printf '\nReal VM smoke test passed.\n'
