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
  "$app" --no-sandbox --disable-gpu > "$base/app.log" 2>&1 &
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

prompt_count="$(grep -c '"kind": "prompt.start"' "$base/fake-acp.log" || true)"
if [ "$prompt_count" -ne 1 ]; then
  echo "FAKE_PROMPT_COUNT=$prompt_count want 1"
  cat "$base/fake-acp.log"
  exit 1
fi
echo "CHAT_OK label=$label session=$session fake_prompts=$prompt_count"

curl -fsS -X POST "http://127.0.0.1:$port/shutdown" >/dev/null 2>&1 || true
kill -TERM "-$app_pid" 2>/dev/null || true
for _ in $(seq 1 20); do
  if ! kill -0 "$app_pid" 2>/dev/null; then break; fi
  sleep 1
done
if kill -0 "$app_pid" 2>/dev/null; then
  echo "APP_DID_NOT_STOP label=$label"
  tail -50 "$base/app.log" || true
  exit 1
fi
echo "STOP_OK label=$label"
