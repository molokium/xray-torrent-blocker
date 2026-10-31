#!/usr/bin/env python3
"""Live integration test against a real PasarGuard panel.

This script is intentionally not part of `go test`: it needs a running panel
and a disposable user. It verifies that tblocker speaks the real v5 API, not
just the fake server used by qa/e2e.py.

Usage:
  PANEL_URL=http://127.0.0.1:8001 \
  PANEL_API_KEY=pg_key_... \
  PANEL_USER_ID=1 \
  python3 qa/live_panel.py
"""

from __future__ import annotations

import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BINARY = ROOT / "build" / "tblocker-qa"
PANEL = os.environ.get("PANEL_URL", "").rstrip("/")
KEY = os.environ.get("PANEL_API_KEY", "")
USER_ID = int(os.environ.get("PANEL_USER_ID", "1"))
USERNAME = os.environ.get("PANEL_USERNAME", "alice")


def api(method: str, path: str, body: dict | None = None) -> dict:
    data = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request(
        PANEL + path,
        data=data,
        method=method,
        headers={"X-Api-Key": KEY, "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            raw = response.read()
            return json.loads(raw) if raw else {}
    except urllib.error.HTTPError as exc:
        raise AssertionError(f"{method} {path} -> HTTP {exc.code}: {exc.read()[:300]!r}") from exc


def status() -> str:
    return api("GET", f"/api/user/by-id/{USER_ID}")["status"]


def set_status(disabled: bool) -> None:
    api("PUT", f"/api/user/by-id/{USER_ID}/disabled", {"disabled": disabled})


def wait_for(predicate, description: str, timeout: float = 15.0) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return
        time.sleep(0.25)
    raise AssertionError(f"timed out waiting for {description} (status={status()})")


def build() -> None:
    BINARY.parent.mkdir(exist_ok=True)
    go = shutil.which("go") or str(Path.home() / ".local" / "go" / "bin" / "go")
    subprocess.run(
        [go, "build", "-trimpath", "-ldflags", "-X main.Version=live", "-o", str(BINARY), "."],
        cwd=ROOT,
        check=True,
    )


class Runner:
    def __init__(self, config: Path) -> None:
        self.out = tempfile.TemporaryFile(mode="w+")
        self.proc = subprocess.Popen(
            [str(BINARY), "-c", str(config)],
            stdout=self.out,
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
        self.out.seek(0)
        self.log = self.out.read()

    def tail(self) -> str:
        self.out.seek(0)
        return self.out.read()[-5000:]


def append_line(path: Path, email: str) -> None:
    with path.open("a") as handle:
        handle.write(
            "2026/10/31 12:00:00 from 203.0.113.9:4567 accepted "
            f"tcp:tracker.example.com:443 [vless-in >> TORRENT] email: {email}\n"
        )


def write_config(path: Path, work: Path, logfile: Path, identifier: str) -> None:
    path.write_text(
        f"""LogFile: "{logfile}"
BlockDuration: 10
TorrentTag: "TORRENT"
StorageDir: "{work}"
Hostname: "live-node"
CheckIntervalSeconds: 5
SendAdminMessage: false
SendUserMessage: false
SendWebhook: false
PasarGuard:
  BaseURL: "{PANEL}"
  APIKey: "{KEY}"
  Identifier: "{identifier}"
  TimeoutSeconds: 10
  StartupCheck: true
""",
        encoding="utf-8",
    )
    path.chmod(0o600)


def rewrite_expiry(work: Path) -> None:
    path = work / "blocks.json"
    data = json.loads(path.read_text())
    for record in data.values():
        record["blocked_until"] = "2020-01-01T00:00:00Z"
    path.write_text(json.dumps(data, indent=2))


def run_case(name: str, email: str, identifier: str) -> None:
    work = Path(tempfile.mkdtemp(prefix=f"tblocker-live-{name}-"))
    logfile = work / "access.log"
    logfile.touch()
    config = work / "config.yaml"
    write_config(config, work, logfile, identifier)
    runner: Runner | None = None
    try:
        runner = Runner(config)
        time.sleep(1.0)
        append_line(logfile, email)
        wait_for(lambda: status() == "disabled", f"{name}: panel status disabled")
        blocks = json.loads((work / "blocks.json").read_text())
        record = next(iter(blocks.values()))
        assert record["state"] == "active", record
        assert record["user_id"] == USER_ID, record
        runner.stop()

        # A restart before expiry must not enable the user.
        runner = Runner(config)
        time.sleep(1.5)
        assert status() == "disabled", f"{name}: user was enabled before expiry"
        runner.stop()

        rewrite_expiry(work)
        runner = Runner(config)
        wait_for(lambda: status() == "active", f"{name}: panel status active after expiry")
        wait_for(lambda: (work / "blocks.json").read_text().strip() == "{}", f"{name}: empty journal")
        runner.stop()
        print(f"{name}: PASS")
        return
    except Exception:
        if runner is not None:
            print(f"---- tblocker output ({name}) ----")
            print(runner.tail())
        raise
    finally:
        if runner is not None and runner.proc.poll() is None:
            runner.stop()
        shutil.rmtree(work, ignore_errors=True)


def main() -> int:
    if not PANEL or not KEY:
        print("PANEL_URL and PANEL_API_KEY are required", file=sys.stderr)
        return 2
    build()

    set_status(False)
    assert status() == "active"
    try:
        run_case("numeric-id", str(USER_ID), "auto")
        set_status(False)
        run_case("username-mode", USERNAME, "username")

        # Already disabled: tblocker must record but never enable it.
        set_status(True)
        work = Path(tempfile.mkdtemp(prefix="tblocker-live-preexisting-"))
        logfile = work / "access.log"
        logfile.touch()
        config = work / "config.yaml"
        write_config(config, work, logfile, "auto")
        runner = Runner(config)
        try:
            time.sleep(1.0)
            append_line(logfile, str(USER_ID))
            wait_for(lambda: (work / "blocks.json").exists(), "pre-existing journal")
            time.sleep(1.0)
            assert status() == "disabled"
            blocks = json.loads((work / "blocks.json").read_text())
            record = next(iter(blocks.values()))
            assert record["pre_existing_disabled"] is True, record
            runner.stop()
            rewrite_expiry(work)
            runner = Runner(config)
            time.sleep(2.0)
            assert status() == "disabled", "already-disabled user was re-enabled"
            assert (work / "blocks.json").read_text().strip() == "{}"
            runner.stop()
            print("pre-existing-disabled: PASS")
        finally:
            if runner.proc.poll() is None:
                runner.stop()
            shutil.rmtree(work, ignore_errors=True)
            set_status(False)
    finally:
        set_status(False)

    print("LIVE PANEL PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
