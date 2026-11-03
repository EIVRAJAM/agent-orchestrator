#!/usr/bin/env bash
# Temporary candidate smoke test. Runs only inside a disposable GitHub runner.
set -euo pipefail

label="${1:?label required}"
app="${2:?app binary required}"
cli="${3:?ao cli required}"
version="${4:?expected version required}"
base="${RUNNER_TEMP:?RUNNER_TEMP required}/ao-smoke-$label"

rm -rf "$base"
mkdir -p "$base/home" "$base/data" "$base/fake/bin"
export HOME="$base/home"
export XDG_DATA_HOME="$HOME/.local/share"
export XDG_CONFIG_HOME="$HOME/.config"
export XDG_CACHE_HOME="$HOME/.cache"
export AO_DATA_DIR="$base/data"
export AO_RUN_FILE="$base/running.json"
export AO_SMOKE_FAKE_LOG="$base/fake-acp.log"
export AO_DISABLE_GPU=1
export ELECTRON_DISABLE_SANDBOX=1
export ELECTRON_ENABLE_LOGGING=1
port="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"
debug_port="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"
export AO_PORT="$port"
export PATH="$base/fake/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export SHELL="$base/fake/fake-shell"

cat > "$base/fake/bin/opencode" <<EOF
#!/bin/sh
set -eu
case "\${1:-}" in
  --version) printf '%s\n' '1.0.0' ;;
  auth) printf '%s\n' 'No credentials configured' ;;
  acp|*) exec python3 "$base/fake/fake_acp.py" "\$@" ;;
esac
EOF

cat > "$base/fake/fake_acp.py" <<'PY'
import json
import os
import sys

SESSION = "smoke-opencode-session"
LOG = os.environ["AO_SMOKE_FAKE_LOG"]


def log(kind, **fields):
    with open(LOG, "a", encoding="utf-8") as fh:
        fh.write(json.dumps({"kind": kind, **fields}, sort_keys=True) + "\n")
        fh.flush()


def send(obj):
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def main():
    log("provider.start", argv=sys.argv[1:])
    for line in sys.stdin:
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue
        mid = msg.get("id")
        method = msg.get("method")
        log("method", method=method)
        if method == "initialize":
            send({"jsonrpc": "2.0", "id": mid, "result": {
                "protocolVersion": 1,
                "agentCapabilities": {"sessionCapabilities": {"resume": {}}},
                "authMethods": [],
            }})
        elif method == "session/new":
            send({"jsonrpc": "2.0", "id": mid, "result": {"sessionId": SESSION}})
        elif method in ("session/resume", "session/load"):
            send({"jsonrpc": "2.0", "id": mid, "result": {"sessionId": SESSION}})
        elif method == "session/prompt":
            send({"jsonrpc": "2.0", "method": "session/update", "params": {
                "sessionId": SESSION,
                "update": {"sessionUpdate": "agent_message_chunk",
                           "content": {"type": "text", "text": "smoke-ok"}},
            }})
            send({"jsonrpc": "2.0", "id": mid, "result": {"stopReason": "end_turn"}})
            log("prompt.completed")
        elif mid is not None:
            send({"jsonrpc": "2.0", "id": mid, "result": {}})
    log("provider.eof")


if __name__ == "__main__":
    main()
PY

cat > "$base/fake/fake-shell" <<'EOF'
#!/bin/sh
printf '%s' '__AO_SHELL_ENV__'
env -0
EOF

chmod 700 "$base/fake/bin/opencode" "$base/fake/fake_acp.py" "$base/fake/fake-shell"

mkdir -p "$base/repo"
git -C "$base/repo" init -q
git -C "$base/repo" config user.email smoke@example.invalid
git -C "$base/repo" config user.name Smoke
printf '# smoke\n' > "$base/repo/README.md"
git -C "$base/repo" add README.md
git -C "$base/repo" commit -qm init
git clone -q --bare "$base/repo" "$base/origin.git"
git -C "$base/repo" remote add origin "$base/origin.git"
git -C "$base/repo" fetch -q origin
git -C "$base/repo" remote set-head origin --auto

setsid env \
  HOME="$HOME" XDG_DATA_HOME="$XDG_DATA_HOME" XDG_CONFIG_HOME="$XDG_CONFIG_HOME" XDG_CACHE_HOME="$XDG_CACHE_HOME" \
  SHELL="$SHELL" PATH="$PATH" AO_DATA_DIR="$AO_DATA_DIR" AO_RUN_FILE="$AO_RUN_FILE" AO_PORT="$AO_PORT" \
  AO_SMOKE_FAKE_LOG="$AO_SMOKE_FAKE_LOG" AO_DISABLE_GPU=1 ELECTRON_DISABLE_SANDBOX=1 \
  ELECTRON_ENABLE_LOGGING=1 DISPLAY="${DISPLAY:?DISPLAY required}" \
  "$app" --no-sandbox --disable-gpu --remote-debugging-port="$debug_port" > "$base/app.log" 2>&1 &
app_pid=$!

cleanup() {
  kill -TERM "-$app_pid" 2>/dev/null || true
  sleep 1
  kill -KILL "-$app_pid" 2>/dev/null || true
}
trap cleanup EXIT

ready=0
for _ in $(seq 1 45); do
  if [ -f "$AO_RUN_FILE" ] && curl -fsS --max-time 2 "http://127.0.0.1:$port/healthz" > "$base/healthz.json"; then
    ready=1
    break
  fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  echo "APP_READY_FAILED label=$label"
  tail -100 "$base/app.log" || true
  exit 1
fi
echo "HEALTHZ_OK label=$label port=$port"
cat "$base/healthz.json"

version_file=""
for _ in $(seq 1 15); do
  version_file="$(find "$base" -name app-state.json -type f -print -quit 2>/dev/null || true)"
  [ -n "$version_file" ] && break
  sleep 1
done
if [ -z "$version_file" ]; then
  echo "APP_STATE_MISSING label=$label"
  find "$base" -maxdepth 4 -type f -printf '%p\n' | head -50 || true
  exit 1
fi
python3 - "$version_file" "$version" <<'PY'
import json
import sys

path, want = sys.argv[1], sys.argv[2]
got = json.load(open(path)).get("version")
print("APP_VERSION", got)
if got != want:
    raise SystemExit(f"app version = {got!r}, want {want!r}")
PY

ao() {
  env HOME="$HOME" XDG_DATA_HOME="$XDG_DATA_HOME" XDG_CONFIG_HOME="$XDG_CONFIG_HOME" XDG_CACHE_HOME="$XDG_CACHE_HOME" \
    SHELL="$SHELL" PATH="$PATH" AO_DATA_DIR="$AO_DATA_DIR" AO_RUN_FILE="$AO_RUN_FILE" AO_PORT="$AO_PORT" \
    "$cli" "$@"
}

ao project add --path "$base/repo" --worker-agent opencode --orchestrator-agent opencode > "$base/project-add.log" 2>&1
spawn_out="$(ao spawn --project repo --agent opencode --kind worker --mode chat --name "smoke-$label" --skip-agent-check 2>&1)"
printf '%s\n' "$spawn_out" > "$base/spawn.log"
session="$(printf '%s\n' "$spawn_out" | sed -n 's/^spawned session \([^ ]*\).*/\1/p' | head -1)"
if [ -z "$session" ]; then
  echo "SPAWN_PARSE_FAILED"
  cat "$base/spawn.log"
  exit 1
fi

controller=""
for _ in $(seq 1 30); do
  controller="$(curl -fsS --max-time 2 "http://127.0.0.1:$port/api/v1/sessions/$session/conversation" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("controller",""))' 2>/dev/null || true)"
  if [ "$controller" = "ready" ]; then break; fi
  sleep 1
done
if [ "$controller" != "ready" ]; then
  echo "CONTROLLER_NOT_READY session=$session"
  curl -sS "http://127.0.0.1:$port/api/v1/sessions/$session/conversation" || true
  exit 1
fi

curl -fsS -X POST "http://127.0.0.1:$port/api/v1/sessions/$session/conversation/messages" \
  -H 'Content-Type: application/json' -d '{"text":"smoke ping"}' > "$base/send.json"
python3 - "$port" "$session" <<'PY'
import json
import sys
import time
import urllib.request

port, sid = sys.argv[1], sys.argv[2]
last = None
for _ in range(45):
    last = json.load(urllib.request.urlopen(f"http://127.0.0.1:{port}/api/v1/sessions/{sid}/conversation"))
    texts = [m.get("text", "") for m in last.get("messages", [])]
    turns = last.get("turns", [])
    if "smoke-ok" in texts and any(t.get("state") == "completed" for t in turns):
        print(json.dumps({"controller": last.get("controller"), "turns": turns, "texts": texts, "queue": last.get("queue")}, indent=2))
        raise SystemExit(0)
    time.sleep(1)
print(json.dumps(last, indent=2))
raise SystemExit(1)
PY

python3 - "$base/fake-acp.log" <<'PY'
import json
import sys

calls = 0
completed = 0
for line in open(sys.argv[1], encoding="utf-8"):
    try:
        record = json.loads(line)
    except json.JSONDecodeError:
        continue
    if record.get("kind") == "method" and record.get("method") == "session/prompt":
        calls += 1
    if record.get("kind") == "prompt.completed":
        completed += 1
print(f"FAKE_SESSION_PROMPTS={calls} FAKE_PROMPT_COMPLETIONS={completed}")
if calls != 1 or completed != 1:
    raise SystemExit("fake ACP must see exactly one session/prompt and one prompt.completed")
PY
echo "CHAT_OK label=$label session=$session fake_prompts=1"

shutdown_ok=0
for _ in $(seq 1 10); do
  if curl -fsS --max-time 3 -X POST "http://127.0.0.1:$port/shutdown" \
    -o "$base/shutdown.body" -D "$base/shutdown.headers"; then
    shutdown_ok=1
    break
  fi
  sleep 1
done
if [ "$shutdown_ok" -ne 1 ]; then
  echo "SHUTDOWN_HTTP_FAILED label=$label"
  cat "$base/shutdown.body" 2>/dev/null || true
  tail -100 "$base/app.log" || true
  exit 1
fi
echo "SHUTDOWN_HTTP_OK label=$label"

# Stop the packaged application through its supported renderer bridge:
# window.ao.menu.action("app.quit") invokes the main-process app.quit handler.
python3 - "$debug_port" <<'PY'
import base64
import json
import os
import socket
import struct
import sys
import time
import urllib.request
from urllib.parse import urlparse

port = sys.argv[1]
target = None
for _ in range(30):
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/json/list", timeout=2) as response:
            targets = json.load(response)
        pages = [item for item in targets if item.get("type") == "page"]
        if pages:
            target = pages[0]
            break
    except Exception:
        pass
    time.sleep(0.5)
if target is None:
    raise SystemExit("NO_DEBUG_PAGE_TARGET")
ws_url = target.get("webSocketDebuggerUrl")
if not ws_url:
    raise SystemExit("NO_WEBSOCKET_DEBUGGER_URL")

u = urlparse(ws_url)
sock = socket.create_connection((u.hostname, u.port), timeout=5)
key = base64.b64encode(os.urandom(16)).decode()
path = u.path + (("?" + u.query) if u.query else "")
request = (
    f"GET {path} HTTP/1.1\r\n"
    f"Host: {u.hostname}:{u.port}\r\n"
    "Upgrade: websocket\r\n"
    "Connection: Upgrade\r\n"
    f"Sec-WebSocket-Key: {key}\r\n"
    "Sec-WebSocket-Version: 13\r\n\r\n"
)
sock.sendall(request.encode())
handshake = b""
while b"\r\n\r\n" not in handshake:
    chunk = sock.recv(4096)
    if not chunk:
        raise SystemExit("WS_HANDSHAKE_EOF")
    handshake += chunk
if b" 101 " not in handshake.split(b"\r\n", 1)[0]:
    raise SystemExit("WS_HANDSHAKE_FAILED")

payload = json.dumps({
    "id": 1,
    "method": "Runtime.evaluate",
    "params": {
        "expression": "window.ao.menu.action('app.quit')",
        "awaitPromise": True,
        "returnByValue": True,
    },
}).encode()
mask = os.urandom(4)
header = bytearray([0x81])
if len(payload) < 126:
    header.append(0x80 | len(payload))
elif len(payload) < 65536:
    header.append(0x80 | 126)
    header.extend(struct.pack("!H", len(payload)))
else:
    header.append(0x80 | 127)
    header.extend(struct.pack("!Q", len(payload)))
header.extend(mask)
header.extend(bytes(byte ^ mask[i % 4] for i, byte in enumerate(payload)))
sock.sendall(header)

def read_frame():
    first = sock.recv(2)
    if len(first) != 2:
        raise SystemExit("WS_FRAME_EOF")
    opcode = first[0] & 0x0F
    length = first[1] & 0x7F
    if length == 126:
        length = struct.unpack("!H", sock.recv(2))[0]
    elif length == 127:
        length = struct.unpack("!Q", sock.recv(8))[0]
    data = b""
    while len(data) < length:
        chunk = sock.recv(length - len(data))
        if not chunk:
            raise SystemExit("WS_FRAME_PAYLOAD_EOF")
        data += chunk
    return opcode, data

while True:
    opcode, data = read_frame()
    if opcode == 8:
        raise SystemExit("WS_CLOSED_BEFORE_QUIT_RESPONSE")
    if opcode != 1:
        continue
    message = json.loads(data.decode())
    if message.get("id") != 1:
        continue
    if message.get("error"):
        raise SystemExit(f"APP_QUIT_RPC_ERROR {message['error']}")
    result = message.get("result") or {}
    if result.get("exceptionDetails"):
        raise SystemExit(f"APP_QUIT_EXCEPTION {json.dumps(result['exceptionDetails'])}")
    print("APP_QUIT_RPC_OK")
    break
sock.close()
PY

app_exited=0
for _ in $(seq 1 30); do
  if ! kill -0 "$app_pid" 2>/dev/null; then
    app_exited=1
    break
  fi
  sleep 1
done
if [ "$app_exited" -ne 1 ]; then
  echo "APP_DID_NOT_EXIT label=$label"
  # Emergency cleanup only after marking the smoke failed.
  kill -TERM "-$app_pid" 2>/dev/null || true
  sleep 2
  kill -KILL "-$app_pid" 2>/dev/null || true
  wait "$app_pid" 2>/dev/null
  tail -100 "$base/app.log" || true
  exit 1
fi

set +e
wait "$app_pid"
app_status=$?
set -e
echo "APP_EXIT_STATUS=$app_status"
if [ "$app_status" -ne 0 ]; then
  echo "APP_EXIT_FAILED label=$label status=$app_status"
  tail -100 "$base/app.log" || true
  exit 1
fi

# Verify no process from this smoke remains, including the daemon/host/provider.
remaining=""
for _ in $(seq 1 15); do
  remaining=""
  self="$$"
  for entry in /proc/[0-9]*; do
    pid="${entry##*/}"
    [ "$pid" = "$self" ] && continue
    [ -r "$entry/environ" ] || continue
    if tr '\0' '\n' < "$entry/environ" 2>/dev/null | grep -Fxq "AO_SMOKE_FAKE_LOG=$base/fake-acp.log"; then
      remaining="$remaining $pid"
    fi
  done
  [ -z "$remaining" ] && break
  sleep 1
done
if [ -n "$remaining" ]; then
  echo "SMOKE_PROCESSES_REMAIN:$remaining"
  # Emergency cleanup only after marking the smoke failed.
  for pid in $remaining; do kill -TERM "$pid" 2>/dev/null || true; done
  sleep 2
  for pid in $remaining; do kill -KILL "$pid" 2>/dev/null || true; done
  exit 1
fi
echo "CLEAN_STOP_OK label=$label"
