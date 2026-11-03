#!/usr/bin/env bash
# Temporary candidate smoke test. Runs only inside a disposable GitHub runner.
set -euo pipefail

label="${1:?label required}"
app="${2:?app binary required}"
cli="${3:?ao cli required}"
version="${4:?expected version required}"
base="${RUNNER_TEMP:?RUNNER_TEMP required}/ao-smoke-$label"
instance="ao-smoke-$label-$$"

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

cat > "$base/inspect_procs.py" <<'PY'
#!/usr/bin/env python3
"""Inspect /proc without ps and account for process inspection races."""

import argparse
import errno
import glob
import json
import os
import sys


class Gone(Exception):
    pass


class InspectError(Exception):
    pass


def read_bytes(path):
    try:
        with open(path, "rb") as fh:
            return fh.read()
    except FileNotFoundError as exc:
        raise Gone(str(exc)) from exc
    except PermissionError as exc:
        raise InspectError(f"permission denied: {path}: {exc}") from exc
    except OSError as exc:
        if exc.errno in (errno.ENOENT, errno.ESRCH):
            raise Gone(str(exc)) from exc
        raise InspectError(f"read failed: {path}: {exc}") from exc


def read_process(pid, marker):
    try:
        stat = read_bytes(f"/proc/{pid}/stat").decode("utf-8", "replace")
    except Gone:
        return None
    close = stat.rfind(")")
    if close < 0:
        raise InspectError(f"malformed stat for pid {pid}")
    fields = stat[close + 2 :].split()
    if len(fields) < 4:
        raise InspectError(f"short stat for pid {pid}: {stat!r}")
    try:
        ppid = int(fields[1])
        pgid = int(fields[2])
        sid = int(fields[3])
    except ValueError as exc:
        raise InspectError(f"non-numeric stat for pid {pid}: {stat!r}") from exc
    name = stat[stat.find("(") + 1 : close]
    state = fields[0]

    environ = b""
    if state != "Z":
        try:
            environ = read_bytes(f"/proc/{pid}/environ")
        except Gone:
            return None
        except InspectError:
            # A process can become a zombie between stat and environ. Re-read
            # stat before deciding whether this is a real inspection failure.
            try:
                retry = read_bytes(f"/proc/{pid}/stat").decode("utf-8", "replace")
            except Gone:
                return None
            retry_close = retry.rfind(")")
            retry_fields = retry[retry_close + 2 :].split() if retry_close >= 0 else []
            if retry_fields and retry_fields[0] == "Z":
                environ = b""
                state = "Z"
            else:
                raise

    raw_cmd = b""
    if state != "Z":
        try:
            raw_cmd = read_bytes(f"/proc/{pid}/cmdline")
        except Gone:
            return None
    cmd = " ".join(
        part.decode("utf-8", "replace")
        for part in raw_cmd.split(b"\0")
        if part
    )
    if not cmd:
        cmd = f"[{name}]"
    marker_value = f"AO_SMOKE_INSTANCE_ID={marker}".encode()
    marker_present = marker_value in environ.split(b"\0")
    if "chat-host" in cmd:
        role = "chat-host"
    elif "fake_acp.py" in cmd:
        role = "fake-provider"
    elif "/resources/daemon/ao" in cmd:
        role = "daemon"
    elif "agent-orchestrator" in cmd:
        role = "app"
    else:
        role = "other"
    return {
        "pid": pid,
        "ppid": ppid,
        "pgid": pgid,
        "sid": sid,
        "state": state,
        "name": name,
        "role": role,
        "marker": marker_present,
        "cmd": cmd,
    }


def load_groups(path, app_pid, required):
    groups = {int(app_pid)}
    try:
        with open(path, encoding="utf-8") as fh:
            data = json.load(fh)
    except FileNotFoundError:
        if required:
            raise InspectError(f"missing known-groups file: {path}")
        return groups
    except (OSError, ValueError) as exc:
        raise InspectError(f"invalid known-groups file {path}: {exc}") from exc
    try:
        groups.update(int(value) for value in data["groups"])
    except (KeyError, TypeError, ValueError) as exc:
        raise InspectError(f"invalid known-groups payload {path}: {data!r}") from exc
    return groups


def scan(marker, app_pid, groups_file, required_groups):
    groups = load_groups(groups_file, app_pid, required_groups)
    records = []
    errors = []
    for entry in glob.glob("/proc/[0-9]*"):
        try:
            pid = int(os.path.basename(entry))
            record = read_process(pid, marker)
        except (ValueError, InspectError) as exc:
            errors.append(str(exc))
            continue
        if record is None:
            continue
        if record["marker"]:
            records.append(record)
        elif record["state"] == "Z" and (
            record["pgid"] in groups or record["sid"] in groups
        ):
            record["role"] = "orphan-zombie"
            records.append(record)
    return records, errors, groups


def render(record):
    kind = "ZOMBIE" if record["state"] == "Z" else "ACTIVE"
    return (
        f"{kind} pid={record['pid']} ppid={record['ppid']} "
        f"pgid={record['pgid']} sid={record['sid']} "
        f"state={record['state']} role={record['role']} "
        f"marker={'1' if record['marker'] else '0'} "
        f"cmd={record['cmd']!r}"
    )


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("snapshot", "check"))
    parser.add_argument("marker")
    parser.add_argument("app_pid", type=int)
    parser.add_argument("groups_file")
    parser.add_argument("--phase", choices=("host", "final"))
    args = parser.parse_args()

    try:
        records, errors, groups = scan(
            args.marker,
            args.app_pid,
            args.groups_file,
            required_groups=args.mode == "check",
        )
    except InspectError as exc:
        print(f"INSPECT_ERROR {exc}")
        return 12

    records.sort(key=lambda item: item["pid"])
    if args.mode == "snapshot":
        if errors:
            print("INSPECT_ERRORS")
            for error in errors:
                print(f"INSPECT_ERROR {error}")
            return 12
        for record in records:
            print(render(record))
        try:
            with open(args.groups_file, "w", encoding="utf-8") as fh:
                json.dump({"groups": sorted(groups)}, fh, sort_keys=True)
        except OSError as exc:
            print(f"INSPECT_ERROR writing {args.groups_file}: {exc}")
            return 12
        active = sum(record["state"] != "Z" for record in records)
        zombies = sum(record["state"] == "Z" for record in records)
        print(
            f"SNAPSHOT_OK active={active} zombies={zombies} groups={len(groups)}"
        )
        return 0

    if args.phase is None:
        print("INSPECT_ERROR missing --phase for check")
        return 12
    if errors:
        print("INSPECT_ERRORS")
        for error in errors:
            print(f"INSPECT_ERROR {error}")
        return 12

    active = [record for record in records if record["state"] != "Z"]
    zombies = [record for record in records if record["state"] == "Z"]
    if args.phase == "host":
        bad_active = [
            record
            for record in active
            if record["role"] in ("chat-host", "fake-provider")
        ]
        if bad_active:
            for record in bad_active:
                print(render(record))
            print(f"HOST_ACTIVE={len(bad_active)}")
            return 10
        if zombies:
            for record in zombies:
                print(render(record))
            print(f"INSTANCE_ZOMBIES={len(zombies)}")
            return 11
        print(f"PROCESS_CHECK_OK phase=host active={len(active)} zombies=0")
        return 0

    if active:
        for record in active:
            print(render(record))
        print(f"INSTANCE_ACTIVE={len(active)}")
        return 10
    if zombies:
        for record in zombies:
            print(render(record))
        print(f"INSTANCE_ZOMBIES={len(zombies)}")
        return 11
    print("PROCESS_CHECK_OK phase=final active=0 zombies=0")
    return 0


if __name__ == "__main__":
    sys.exit(main())
PY
chmod 700 "$base/inspect_procs.py"

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

app_pid=""
app_exited=0
setsid env \
  HOME="$HOME" XDG_DATA_HOME="$XDG_DATA_HOME" XDG_CONFIG_HOME="$XDG_CONFIG_HOME" XDG_CACHE_HOME="$XDG_CACHE_HOME" \
  SHELL="$SHELL" PATH="$PATH" AO_DATA_DIR="$AO_DATA_DIR" AO_RUN_FILE="$AO_RUN_FILE" AO_PORT="$AO_PORT" \
  AO_SMOKE_INSTANCE_ID="$instance" AO_SMOKE_FAKE_LOG="$AO_SMOKE_FAKE_LOG" \
  AO_DISABLE_GPU=1 ELECTRON_DISABLE_SANDBOX=1 ELECTRON_ENABLE_LOGGING=1 \
  DISPLAY="${DISPLAY:?DISPLAY required}" \
  "$app" --no-sandbox --disable-gpu --remote-debugging-port="$debug_port" --remote-allow-origins='*' > "$base/app.log" 2>&1 &
app_pid=$!

cleanup() {
  if [ "$app_exited" -eq 1 ] || [ -z "$app_pid" ]; then
    return
  fi
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

# Record the process groups/sessions belonging to this smoke before the host
# exits, so orphaned zombies can still be attributed to this instance.
if ! python3 "$base/inspect_procs.py" snapshot "$instance" "$app_pid" "$base/known-groups.json" > "$base/processes-before-kill.txt"; then
  echo "PROCESS_SNAPSHOT_FAILED label=$label"
  cat "$base/processes-before-kill.txt" || true
  exit 1
fi
cat "$base/processes-before-kill.txt"

# Terminate the disposable session through its supported CLI. This explicitly
# gives the detached ACP host permission to close instead of preserving it.
if ! ao session kill "$session" > "$base/session-kill.log" 2>&1; then
  echo "SESSION_KILL_FAILED label=$label"
  cat "$base/session-kill.log" || true
  exit 1
fi
echo "SESSION_KILL_OK label=$label"

host_status=1
for _ in $(seq 1 20); do
  set +e
  python3 "$base/inspect_procs.py" check "$instance" "$app_pid" "$base/known-groups.json" --phase host > "$base/host-check.txt" 2>&1
  host_status=$?
  set -e
  if [ "$host_status" -eq 0 ] || [ "$host_status" -eq 12 ]; then
    break
  fi
  sleep 1
done
if [ "$host_status" -ne 0 ]; then
  echo "SESSION_HOST_NOT_STOPPED label=$label status=$host_status"
  cat "$base/host-check.txt" || true
  exit 1
fi
cat "$base/host-check.txt"
echo "SESSION_HOST_STOPPED label=$label"

if ! curl -fsS --max-time 3 "http://127.0.0.1:$port/healthz" > /dev/null; then
  echo "DAEMON_UNEXPECTEDLY_STOPPED label=$label"
  exit 1
fi
echo "DAEMON_ALIVE_BEFORE_SHUTDOWN label=$label"

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

def recv_exact(size):
    data = b""
    while len(data) < size:
        chunk = sock.recv(size - len(data))
        if not chunk:
            raise EOFError
        data += chunk
    return data


def read_frame():
    first = recv_exact(2)
    opcode = first[0] & 0x0F
    length = first[1] & 0x7F
    if length == 126:
        length = struct.unpack("!H", recv_exact(2))[0]
    elif length == 127:
        length = struct.unpack("!Q", recv_exact(8))[0]
    return opcode, recv_exact(length)


try:
    while True:
        opcode, data = read_frame()
        if opcode == 8:
            print("APP_QUIT_CDP_EOF")
            break
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
except EOFError:
    # The app may close the browser websocket while quitting after the full
    # request frame was sent. The process exit status is still authoritative.
    print("APP_QUIT_CDP_EOF")
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

final_status=1
for _ in $(seq 1 15); do
  set +e
  python3 "$base/inspect_procs.py" check "$instance" "$app_pid" "$base/known-groups.json" --phase final > "$base/final-check.txt" 2>&1
  final_status=$?
  set -e
  if [ "$final_status" -eq 0 ] || [ "$final_status" -eq 12 ]; then
    break
  fi
  sleep 1
done
if [ "$final_status" -ne 0 ]; then
  echo "SMOKE_PROCESSES_REMAIN label=$label status=$final_status"
  cat "$base/final-check.txt" || true
  exit 1
fi
cat "$base/final-check.txt"
echo "CLEAN_STOP_OK label=$label"
