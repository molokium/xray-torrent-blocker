#!/usr/bin/env python3
"""Black-box end-to-end test for tblocker against a fake PasarGuard panel.

Run from the repository root:  python3 qa/e2e.py
Requires Go on PATH to build the binary.
"""

from __future__ import annotations

import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BINARY = ROOT / "build" / "tblocker-qa"


class State:
    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.status = "active"
        self.user_id = 42
        self.username = "alice"
        self.note = "tgid-777"
        self.disable_calls: list[dict] = []
        self.enable_calls: list[dict] = []
        self.telegram: list[dict] = []
        self.webhooks: list[dict] = []
        self.unknown: list[str] = []
        self.api_key_seen: list[str] = []

    def user_json(self) -> dict:
        return {
            "id": self.user_id,
            "username": self.username,
            "status": self.status,
            "note": self.note,
        }


STATE = State()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args) -> None:  # silence
        pass

    def _json(self, code: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _body(self) -> bytes:
        length = int(self.headers.get("Content-Length", "0"))
        return self.rfile.read(length) if length else b""

    def do_GET(self) -> None:
        path = urllib.parse.urlparse(self.path).path
        with STATE.lock:
            STATE.api_key_seen.append(self.headers.get("X-Api-Key", ""))
        if path == "/api/admin":
            self._json(200, {"id": 1, "username": "owner", "status": "active"})
        elif path == f"/api/user/by-id/{STATE.user_id}":
            with STATE.lock:
                self._json(200, STATE.user_json())
        elif path.startswith("/api/user/by-username/"):
            self._json(404, {"detail": "User not found"})
        else:
            with STATE.lock:
                STATE.unknown.append(f"GET {path}")
            self._json(404, {"detail": "not found"})

    def do_PUT(self) -> None:
        path = urllib.parse.urlparse(self.path).path
        raw = self._body()
        if path == f"/api/user/by-id/{STATE.user_id}/disabled":
            try:
                payload = json.loads(raw or b"{}")
            except json.JSONDecodeError:
                self._json(400, {"detail": "bad json"})
                return
            disabled = bool(payload.get("disabled"))
            with STATE.lock:
                STATE.status = "disabled" if disabled else "active"
                call = {"path": path, "body": payload, "api_key": self.headers.get("X-Api-Key", "")}
                (STATE.disable_calls if disabled else STATE.enable_calls).append(call)
                user = STATE.user_json()
            self._json(200, user)
            return
        with STATE.lock:
            STATE.unknown.append(f"PUT {path}")
        self._json(404, {"detail": "not found"})

    def do_POST(self) -> None:
        path = urllib.parse.urlparse(self.path).path
        raw = self._body()
        if path.startswith("/bot") and path.endswith("/sendMessage"):
            form = urllib.parse.parse_qs(raw.decode(), keep_blank_values=True)
            with STATE.lock:
                STATE.telegram.append(
                    {
                        "path": path,
                        "chat_id": form.get("chat_id", [""])[0],
                        "text": form.get("text", [""])[0],
                        "parse_mode": form.get("parse_mode", [""])[0],
                    }
                )
            self._json(200, {"ok": True})
            return
        if path == "/webhook":
            try:
                payload = json.loads(raw)
            except json.JSONDecodeError:
                payload = {"raw": raw.decode(errors="replace")}
            with STATE.lock:
                STATE.webhooks.append(payload)
            self._json(200, {"ok": True})
            return
        with STATE.lock:
            STATE.unknown.append(f"POST {path}")
        self._json(404, {"detail": "not found"})


def start_server() -> ThreadingHTTPServer:
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def wait_for(predicate, description: str, timeout: float = 10.0) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return
        time.sleep(0.1)
    raise AssertionError(f"timed out waiting for {description}")


def write_config(path: Path, base_url: str, storage: Path, logfile: Path) -> None:
    path.write_text(
        f"""LogFile: "{logfile}"
BlockDuration: 10
TorrentTag: "TORRENT"
DetectionThreshold: 10
DetectionWindowMinutes: 5
StorageDir: "{storage}"
Hostname: "e2e-node"
CheckIntervalSeconds: 5
TidRegex: 'tgid-(-?[0-9]+)'
SendAdminMessage: true
AdminBotToken: "111:admin"
AdminChatID: "999"
SendUserMessage: true
BotToken: "222:user"
TelegramAPIURL: "{base_url}"
SendWebhook: true
WebhookURL: "{base_url}/webhook"
WebhookHeaders:
  Authorization: "Bearer e2e"
PasarGuard:
  BaseURL: "{base_url}"
  APIKey: "pg_key_11111111-1111-4111-8111-111111111111"
  Identifier: "auto"
  TimeoutSeconds: 5
  StartupCheck: true
""",
        encoding="utf-8",
    )
    path.chmod(0o600)


class Runner:
    def __init__(self, config: Path) -> None:
        self.config = config
        self.output = tempfile.TemporaryFile(mode="w+")
        self.proc = subprocess.Popen(
            [str(BINARY), "-c", str(config)],
            stdout=self.output,
            stderr=subprocess.STDOUT,
            text=True,
        )

    def stop(self) -> None:
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGTERM)
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=5)
        self.output.seek(0)
        self.log = self.output.read()

    def tail(self) -> str:
        self.output.seek(0)
        return self.output.read()[-4000:]


def append_line(path: Path) -> None:
    line = (
        "2026/10/31 12:00:00 from 203.0.113.9:4567 accepted "
        "tcp:tracker.example.com:443 [vless-in >> TORRENT] email: 42\n"
    )
    with path.open("a", encoding="utf-8") as handle:
        handle.write(line)


def rewrite_expiry(storage: Path, past: bool) -> None:
    path = storage / "blocks.json"
    data = json.loads(path.read_text())
    for record in data.values():
        record["blocked_until"] = "2020-01-01T00:00:00Z" if past else "2030-01-01T00:00:00Z"
    path.write_text(json.dumps(data, indent=2))


def build() -> None:
    BINARY.parent.mkdir(exist_ok=True)
    env = dict(os.environ)
    go = shutil.which("go") or str(Path.home() / ".local" / "go" / "bin" / "go")
    subprocess.run(
        [go, "build", "-trimpath", "-ldflags", "-X main.Version=e2e", "-o", str(BINARY), "."],
        cwd=ROOT,
        check=True,
        env=env,
    )


def main() -> int:
    build()

    server = start_server()
    base_url = f"http://127.0.0.1:{server.server_port}"
    work = Path(tempfile.mkdtemp(prefix="tblocker-e2e-"))
    storage = work / "storage"
    storage.mkdir()
    storage.chmod(0o750)
    logfile = work / "access.log"
    logfile.touch()
    logfile.chmod(0o600)
    config = work / "config.yaml"
    write_config(config, base_url, storage, logfile)

    runner: Runner | None = None
    try:
        # --- 1. block an active user -------------------------------------
        runner = Runner(config)
        time.sleep(0.8)
        # Detection tolerance: the first nine lines must not block; the tenth
        # crosses the configured threshold.
        for _ in range(10):
            append_line(logfile)

        wait_for(lambda: len(STATE.disable_calls) == 1, "disable call")
        call = STATE.disable_calls[0]
        assert call["body"] == {"disabled": True}, call
        assert call["api_key"].startswith("pg_key_"), call
        assert call["path"] == "/api/user/by-id/42/disabled", call

        wait_for(lambda: len(STATE.telegram) == 2, "two Telegram messages")
        chats = {m["chat_id"]: m for m in STATE.telegram}
        assert "999" in chats and "777" in chats, chats
        assert "alice" in chats["999"]["text"], chats["999"]
        assert "203.0.113.9" in chats["999"]["text"], chats["999"]

        wait_for(lambda: len(STATE.webhooks) == 1, "block webhook")
        assert STATE.webhooks[0]["action"] == "block", STATE.webhooks
        assert STATE.webhooks[0]["username"] == "alice", STATE.webhooks

        blocks = json.loads((storage / "blocks.json").read_text())
        assert len(blocks) == 1, blocks
        record = next(iter(blocks.values()))
        assert record["state"] == "active", record
        assert record["previous_status"] == "active", record

        # A second torrent line must slide the expiry forward without another
        # panel call or notification.
        first_until = datetime.fromisoformat(record["blocked_until"])
        append_line(logfile)

        def extended() -> bool:
            data = json.loads((storage / "blocks.json").read_text())
            current = next(iter(data.values()))
            return datetime.fromisoformat(current["blocked_until"]) > first_until

        wait_for(extended, "journal expiry extension")
        assert len(STATE.disable_calls) == 1, STATE.disable_calls
        assert len(STATE.telegram) == 2, STATE.telegram
        assert len(STATE.webhooks) == 1, STATE.webhooks

        runner.stop()
        assert runner.proc.returncode in (0, -signal.SIGTERM), runner.proc.returncode

        # --- 2. restart with an expired record -> enable ------------------
        rewrite_expiry(storage, past=True)
        runner = Runner(config)
        wait_for(lambda: len(STATE.enable_calls) == 1, "enable call after restart")
        assert STATE.enable_calls[0]["body"] == {"disabled": False}, STATE.enable_calls[0]
        wait_for(lambda: len(STATE.webhooks) == 2, "unblock webhook")
        assert STATE.webhooks[1]["action"] == "unblock", STATE.webhooks
        wait_for(lambda: (storage / "blocks.json").read_text().strip() == "{}", "empty store")
        runner.stop()

        # --- 3. already-disabled user is never re-enabled -----------------
        with STATE.lock:
            STATE.status = "disabled"
            STATE.disable_calls.clear()
            STATE.enable_calls.clear()
            STATE.telegram.clear()
            STATE.webhooks.clear()
        runner = Runner(config)
        time.sleep(0.8)
        for _ in range(10):
            append_line(logfile)
        wait_for(lambda: len(STATE.telegram) == 2, "pre-existing block notification")
        assert len(STATE.disable_calls) == 0, STATE.disable_calls
        blocks = json.loads((storage / "blocks.json").read_text())
        record = next(iter(blocks.values()))
        assert record["pre_existing_disabled"] is True, record
        runner.stop()

        rewrite_expiry(storage, past=True)
        runner = Runner(config)
        time.sleep(1.5)
        assert len(STATE.enable_calls) == 0, STATE.enable_calls
        blocks = json.loads((storage / "blocks.json").read_text())
        assert blocks == {}, blocks
        runner.stop()

        assert STATE.unknown == [], STATE.unknown
        print("E2E PASS")
        return 0
    except Exception:
        if runner is not None:
            try:
                print("---- tblocker output ----")
                print(runner.tail())
            except Exception:
                pass
        raise
    finally:
        if runner is not None and runner.proc.poll() is None:
            runner.stop()
        server.shutdown()
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
