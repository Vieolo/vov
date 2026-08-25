#!/usr/bin/env python3
"""Deterministic verification for the vov framework and its examples.

Usage:
    python3 agent.py verify   # build, vet, and test every module
    python3 agent.py smoke    # run the example end-to-end (routes + shutdown)

Per the repo rules, run this instead of re-running the go/curl commands by hand.
Each module is checked independently because `go ... ./...` never crosses a
module boundary, so a break in one module stays invisible to the others.
"""
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent

# (label, module directory relative to ROOT). Order is fixed for determinism.
MODULES = [
    ("vov", "."),
    ("examples/tasks", "examples/tasks"),
]

# (stage name, argv). Build before vet before test.
STAGES = [
    ("build", ["go", "build", "./..."]),
    ("vet", ["go", "vet", "./..."]),
    ("test", ["go", "test", "./..."]),
]


# --- verify -----------------------------------------------------------------

def run_stage(label: str, mod_dir: str, stage_name: str, cmd: list[str]) -> bool:
    where = ROOT / mod_dir
    print(f"-> {label} :: {' '.join(cmd)}")
    completed = subprocess.run(cmd, cwd=where)
    ok = completed.returncode == 0
    print(f"   {'PASS' if ok else 'FAIL'}: {label} :: {stage_name}")
    return ok


def verify() -> int:
    failures: list[str] = []
    for label, mod_dir in MODULES:
        for stage_name, cmd in STAGES:
            if not run_stage(label, mod_dir, stage_name, cmd):
                failures.append(f"{label} :: {stage_name}")
    print()
    if failures:
        print(f"VERIFY FAILED ({len(failures)}):")
        for f in failures:
            print(f"  - {f}")
        return 1
    print("VERIFY PASSED")
    return 0


# --- smoke ------------------------------------------------------------------

SMOKE_PORT = 8080
SMOKE_BASE = f"http://127.0.0.1:{SMOKE_PORT}"
SMOKE_SHUTDOWN_WAIT = 20  # seconds; must exceed the app's ShutdownTimeout


def _http(path: str, method: str = "GET", data: bytes | None = None):
    req = urllib.request.Request(SMOKE_BASE + path, method=method, data=data)
    try:
        with urllib.request.urlopen(req, timeout=3) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:  # 4xx/5xx are results here, not errors
        return e.code, e.read().decode()


def _port_free(port: int) -> bool:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        try:
            s.bind(("127.0.0.1", port))
            return True
        except OSError:
            return False


def _wait_for_bind(proc: subprocess.Popen, timeout: float = 15.0) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if proc.poll() is not None:
            return False  # the process exited before binding
        try:
            _http("/healthz")
            return True
        except (urllib.error.URLError, ConnectionError, OSError):
            time.sleep(0.1)
    return False


def smoke() -> int:
    example = ROOT / "examples" / "tasks"
    if not _port_free(SMOKE_PORT):
        print(f"SMOKE FAILED: port {SMOKE_PORT} is in use")
        return 1

    tmp = tempfile.mkdtemp(prefix="vov-smoke-")
    binpath = str(Path(tmp) / "tasks")
    failures: list[str] = []
    try:
        if subprocess.run(["go", "build", "-o", binpath, "."], cwd=example).returncode != 0:
            print("SMOKE FAILED: example did not build")
            return 1

        proc = subprocess.Popen(
            [binpath], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True
        )
        try:
            if not _wait_for_bind(proc):
                print("SMOKE FAILED: server did not start")
                if proc.poll() is not None:
                    print(proc.communicate(timeout=5)[0])
                return 1

            # (label, actual_status, body, want_status)
            checks = [
                ("GET  /healthz", *_http("/healthz"), 200),
                ("POST /tasks", *_http("/tasks", "POST", b'{"title":"write the tests"}'), 201),
                ("POST /tasks (no title)", *_http("/tasks", "POST", b"{}"), 400),
                ("GET  /tasks/1", *_http("/tasks/1"), 200),
                ("GET  /tasks/99", *_http("/tasks/99"), 404),
                ("GET  /version (escape hatch)", *_http("/version"), 200),
                ("POST /healthz (wrong method)", *_http("/healthz", "POST", b""), 405),
            ]
            for label, status, _body, want in checks:
                ok = status == want
                print(f"   {'ok ' if ok else 'BAD'} {label} -> {status} (want {want})")
                if not ok:
                    failures.append(f"{label}: got {status}, want {want}")

            # Graceful shutdown: SIGTERM should drain, run the hook, and exit 0.
            proc.send_signal(signal.SIGTERM)
            try:
                out, _ = proc.communicate(timeout=SMOKE_SHUTDOWN_WAIT)
            except subprocess.TimeoutExpired:
                proc.kill()
                out, _ = proc.communicate()
                failures.append("server did not shut down within timeout")
            rc = proc.returncode
            hook_ran = "in memory at exit" in (out or "")
            if rc != 0:
                failures.append(f"exit code {rc}, want 0")
            if not hook_ran:
                failures.append("shutdown hook did not run")
            print(f"   {'ok ' if rc == 0 and hook_ran else 'BAD'} SIGTERM -> exit {rc}, hook_ran={hook_ran}")
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    print()
    if failures:
        print(f"SMOKE FAILED ({len(failures)}):")
        for f in failures:
            print(f"  - {f}")
        return 1
    print("SMOKE PASSED")
    return 0


# --- entry ------------------------------------------------------------------

def main() -> int:
    args = sys.argv[1:]
    cmd = args[0] if args else "verify"
    if cmd == "verify":
        return verify()
    if cmd == "smoke":
        return smoke()
    print(f"unknown command: {cmd!r}; supported: verify, smoke", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main())
