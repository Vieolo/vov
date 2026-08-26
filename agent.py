#!/usr/bin/env python3
"""Deterministic verification for the vov framework and its examples.

Usage:
    python3 agent.py verify    # build, vet, test, and gofmt every module
    python3 agent.py smoke     # run the example end-to-end (routes + shutdown)
    python3 agent.py manifest  # check the checked-in route manifest is current
    python3 agent.py manifest --write   # regenerate it after a deliberate change

Per the repo rules, run this instead of re-running the go/curl commands by hand.
Each module is checked independently because `go ... ./...` never crosses a
module boundary, so a break in one module stays invisible to the others.
"""
import difflib
import json
import os
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
    ("fmt", ["gofmt", "-l", "."]),  # see run_stage: any output here is a failure
]


# --- verify -----------------------------------------------------------------

def run_stage(label: str, mod_dir: str, stage_name: str, cmd: list[str]) -> bool:
    where = ROOT / mod_dir
    print(f"-> {label} :: {' '.join(cmd)}")
    if stage_name == "fmt":
        # gofmt -l exits 0 even when it lists unformatted files, so the output
        # is the result: any filename printed means the stage failed.
        completed = subprocess.run(cmd, cwd=where, capture_output=True, text=True)
        listed = completed.stdout.strip()
        if listed:
            print(listed)
        ok = completed.returncode == 0 and not listed
    else:
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


JSON_HDR = {"Content-Type": "application/json"}
AUTH_HDR = {"Authorization": "Bearer t-ramtin"}          # member + tasks.write
ADMIN_HDR = {"Authorization": "Bearer t-ramtin-admin"}   # also role admin
OWNER_HDR = {"Authorization": "Bearer t-ramtin-owner"}   # role owner + tasks.write
PRO_HDR = {"Authorization": "Bearer t-ramtin-pro"}       # member, paid tier 2
HALF_HDR = {"Authorization": "Bearer t-ramtin-halfadmin"} # role admin, no perm
READER_HDR = {"Authorization": "Bearer t-ramtin-reader"} # member, no tasks.write
BOOM_HDR = {"Authorization": "Bearer t-boom"}            # authenticator fails
JSON_AUTH = {**JSON_HDR, **AUTH_HDR}

# The example reads these at startup; TASKS_TOKEN is declared required.
SMOKE_ENV = {
    **os.environ,
    "TASKS_TOKEN": "t-ramtin",
    "TASKS_GREETING": "env-ok",
}


def _http(path: str, method: str = "GET", data: bytes | None = None, headers: dict | None = None):
    """Return (status, headers, body). 4xx/5xx are results here, not exceptions."""
    req = urllib.request.Request(SMOKE_BASE + path, method=method, data=data, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=3) as resp:
            return resp.status, dict(resp.headers), resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read().decode()


def _port_free(port: int) -> bool:
    """True when nothing is listening on the port.

    This connects rather than binds: a plain bind() without SO_REUSEADDR fails
    while sockets from a previous run sit in TIME_WAIT, which would report the
    port busy even though the server — which does set SO_REUSEADDR — can take it.
    """
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.settimeout(0.5)
        return s.connect_ex(("127.0.0.1", port)) != 0


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

        # A required env var that is missing must stop the server from starting,
        # and the error must name the variable without echoing any value.
        bare_env = {k: v for k, v in SMOKE_ENV.items() if k != "TASKS_TOKEN"}
        done = subprocess.run(
            [binpath], env=bare_env, capture_output=True, text=True, timeout=30
        )
        env_ok = done.returncode != 0 and "TASKS_TOKEN" in (done.stdout + done.stderr)
        print(f"   {'ok ' if env_ok else 'BAD'} missing required env aborts boot: "
              f"exit={done.returncode} names_var={'TASKS_TOKEN' in (done.stdout + done.stderr)}")
        if not env_ok:
            failures.append("missing required env var did not abort startup")

        proc = subprocess.Popen(
            [binpath], env=SMOKE_ENV, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True
        )
        try:
            if not _wait_for_bind(proc):
                print("SMOKE FAILED: server did not start")
                if proc.poll() is not None:
                    print(proc.communicate(timeout=5)[0])
                return 1

            def check(label: str, got, want):
                ok = got == want
                print(f"   {'ok ' if ok else 'BAD'} {label}: {got} (want {want})")
                if not ok:
                    failures.append(f"{label}: got {got}, want {want}")

            # --- auth: required by default, escaped explicitly ---------------
            # Declares nothing about auth, so it is protected.
            status, hdrs, _ = _http("/tasks")
            check("GET  /tasks (no credentials)", status, 401)
            # The guard runs inside the middleware chain, so a 401 is still
            # stamped by the default stack (and logged).
            check("     401 still has request id", "X-Request-Id" in hdrs, True)

            # A failing authenticator is a broken dependency, not a bad password.
            status, _, _ = _http("/tasks", headers=BOOM_HDR)
            check("GET  /tasks (authenticator fails)", status, 500)

            status, hdrs, _ = _http("/tasks", headers=AUTH_HDR)
            check("GET  /tasks (authenticated)", status, 200)
            check("     /tasks inherits defaults", "X-Request-Id" in hdrs, True)

            # --- the after-auth phase ----------------------------------------
            # auditLog runs inside the guard, so it can read the resolved user.
            check("     after-auth sees the user", hdrs.get("X-Audit-User"), "ramtin")

            # Rejected before the guard passed, so the after-auth phase never ran.
            _, hdrs, _ = _http("/tasks")
            check("     401 skips after-auth", "X-Audit-User" in hdrs, False)

            # --- routing and handler behavior --------------------------------
            status, hdrs, body = _http("/tasks", "POST", b'{"title":"write the tests"}', JSON_AUTH)
            check("POST /tasks", status, 201)
            # Extend keeps the default stack, so the request id is still stamped.
            check("     /tasks POST keeps defaults", "X-Request-Id" in hdrs, True)
            # The handler read the user out of the request context.
            check("     task owner from context", json.loads(body).get("owner"), "ramtin")

            status, _, _ = _http("/tasks", "POST", b"{}", JSON_AUTH)
            check("POST /tasks (no title)", status, 400)

            # The extra middleware layer this endpoint added on top of the defaults.
            status, _, _ = _http("/tasks", "POST", b"nope", {**AUTH_HDR, "Content-Type": "text/plain"})
            check("POST /tasks (not JSON)", status, 415)

            status, _, _ = _http("/tasks/1", headers=AUTH_HDR)
            check("GET  /tasks/1", status, 200)
            status, _, _ = _http("/tasks/99", headers=AUTH_HDR)
            check("GET  /tasks/99", status, 404)

            # --- roles and permissions ---------------------------------------
            # Authenticated but lacking tasks.write: known, and still refused.
            status, _, _ = _http("/tasks", "POST", b'{"title":"nope"}',
                                 {**JSON_HDR, **READER_HDR})
            check("POST /tasks (no permission)", status, 403)
            # DELETE /tasks/{id} needs a role AND a permission.
            # Has the permission, lacks the role:
            status, _, _ = _http("/tasks/1", "DELETE", headers=AUTH_HDR)
            check("DEL  /tasks/1 (no role)", status, 403)
            # Has the role, lacks the permission — roles and permissions are AND:
            status, _, _ = _http("/tasks/1", "DELETE", headers=HALF_HDR)
            check("DEL  /tasks/1 (role but no permission)", status, 403)
            # ...but GET on the same URL needs neither.
            status, _, _ = _http("/tasks/1", headers=AUTH_HDR)
            check("GET  /tasks/1 (same URL, no role needed)", status, 200)

            # --- the paywall, and the order refusals are decided in ----------
            # No credentials at all is 401 — never 402, which would advertise a
            # price to someone who has not even identified themselves.
            status, _, _ = _http("/reports")
            check("GET  /reports (no credentials)", status, 401)
            # Lacks the required role. Paying would not help, so 403, not 402.
            status, _, _ = _http("/reports", headers=OWNER_HDR)
            check("GET  /reports (wrong role, unpaid)", status, 403)
            # Clears every other requirement and is merely unsubscribed: 402.
            status, _, _ = _http("/reports", headers=READER_HDR)
            check("GET  /reports (right role, unpaid)", status, 402)
            # Paid.
            status, _, _ = _http("/reports", headers=PRO_HDR)
            check("GET  /reports (paid tier 2)", status, 200)

            # --- one URL, several methods ------------------------------------
            # /tasks/{id} declares GET and DELETE in a single Route.
            status, _, _ = _http("/tasks/1", "DELETE", headers=ADMIN_HDR)
            check("DEL  /tasks/1 (admin)", status, 204)
            # "owner" is the second of the any-of roles: also allowed.
            status, _, _ = _http("/tasks", "POST", b'{"title":"by owner"}', {**JSON_HDR, **OWNER_HDR})
            check("POST /tasks (owner)", status, 201)
            status, _, _ = _http("/tasks/2", "DELETE", headers=OWNER_HDR)
            check("DEL  /tasks/2 (owner, any-of role)", status, 204)
            status, _, _ = _http("/tasks/1", headers=AUTH_HDR)
            check("GET  /tasks/1 (after delete)", status, 404)
            # Unauthenticated is 401, not 403: vov does not know who you are.
            status, _, _ = _http("/tasks/2", "DELETE")
            check("DEL  /tasks/2 (no credentials)", status, 401)
            # A method the Route does not declare: net/http derives 405 + Allow
            # from the methods it does.
            status, hdrs, _ = _http("/tasks/1", "PUT", b"{}", JSON_AUTH)
            check("PUT  /tasks/1 (undeclared)", status, 405)
            # HEAD comes along with GET: net/http serves it from the GET handler.
            check("     405 lists the declared methods",
                  sorted(hdrs.get("Allow", "").replace(" ", "").split(",")),
                  ["DELETE", "GET", "HEAD"])

            # --- opted-out routes --------------------------------------------
            status, hdrs, body = _http("/healthz")
            check("GET  /healthz (no credentials)", status, 200)
            # TASKS_GREETING overrode the built-in default and reached a handler.
            check("     env value reached the handler", json.loads(body).get("status"), "env-ok")
            # Default stack dropped via NoMiddleware() -> no request id.
            check("     /healthz is bare", "X-Request-Id" in hdrs, False)
            # NoAuth: there is no user, so the after-auth phase is skipped.
            check("     /healthz skips after-auth", "X-Audit-User" in hdrs, False)

            # The "webhook" stack: its own signature check in Pre, and it shares
            # the request id / logging that the default stack also uses.
            status, hdrs, _ = _http("/webhook", "POST", b"{}", JSON_HDR)
            check("POST /webhook (no signature)", status, 401)
            check("     webhook stack runs its Pre", "X-Request-Id" in hdrs, True)
            # NoAuth, so the stack's Post half never runs.
            check("     webhook stack skips Post", "X-Audit-User" in hdrs, False)

            status, _, _ = _http("/webhook", "POST", b"{}", {**JSON_HDR, "X-Signature": "sig"})
            check("POST /webhook (signed, no credentials)", status, 200)

            status, hdrs, _ = _http("/version")
            check("GET  /version (escape hatch)", status, 200)
            check("     /version has no middleware", "X-Request-Id" in hdrs, False)

            status, _, _ = _http("/healthz", "POST", b"")
            check("POST /healthz (wrong method)", status, 405)

            # Graceful shutdown: SIGTERM should drain, run the hook, and exit 0.
            proc.send_signal(signal.SIGTERM)
            try:
                out, _ = proc.communicate(timeout=SMOKE_SHUTDOWN_WAIT)
            except subprocess.TimeoutExpired:
                proc.kill()
                out, _ = proc.communicate()
                failures.append("server did not shut down within timeout")
            rc = proc.returncode
            log = out or ""
            hook_ran = "tasks_in_memory" in log
            # The dependency struct reached a handler: createTask used both the
            # logger and the S3 client it was handed.
            deps_reached = "archived" in log and "s3://" in log
            print(f"   {'ok ' if deps_reached else 'BAD'} handler used injected deps: {deps_reached}")
            if not deps_reached:
                failures.append("handler did not reach its injected dependencies")
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


# --- manifest ---------------------------------------------------------------

MANIFEST_PATH = ROOT / "examples" / "tasks" / "routes.txt"


def manifest(write: bool = False) -> int:
    """Compare the example's live route manifest with the checked-in file.

    This is the point of the manifest: a policy that is quietly loosened still
    passes every test, because the tests assert against the declaration that
    changed. It only shows up as a diff here.
    """
    example = ROOT / "examples" / "tasks"
    done = subprocess.run(
        ["go", "run", ".", "-manifest"],
        cwd=example, env=SMOKE_ENV, capture_output=True, text=True, timeout=120,
    )
    if done.returncode != 0:
        print("MANIFEST FAILED: could not render the manifest")
        print(done.stdout + done.stderr)
        return 1
    current = done.stdout

    if write:
        MANIFEST_PATH.write_text(current)
        print(f"MANIFEST WRITTEN: {MANIFEST_PATH.relative_to(ROOT)}")
        return 0

    if not MANIFEST_PATH.exists():
        print(f"MANIFEST FAILED: {MANIFEST_PATH.relative_to(ROOT)} does not exist "
              f"(run: python3 agent.py manifest --write)")
        return 1

    checked_in = MANIFEST_PATH.read_text()
    if checked_in == current:
        print(f"   ok  route manifest matches {MANIFEST_PATH.relative_to(ROOT)}")
        print("\nMANIFEST PASSED")
        return 0

    print(f"MANIFEST FAILED: routes changed but {MANIFEST_PATH.relative_to(ROOT)} was not updated.")
    print("Review this diff — a loosened policy looks exactly like any other line.\n")
    diff = difflib.unified_diff(
        checked_in.splitlines(keepends=True), current.splitlines(keepends=True),
        fromfile="routes.txt (checked in)", tofile="routes.txt (current code)",
    )
    print("".join(diff))
    print("If the change is intended: python3 agent.py manifest --write")
    return 1


# --- entry ------------------------------------------------------------------

def main() -> int:
    args = sys.argv[1:]
    cmd = args[0] if args else "verify"
    if cmd == "verify":
        return verify()
    if cmd == "smoke":
        return smoke()
    if cmd == "manifest":
        return manifest(write="--write" in args[1:])
    if cmd == "all":
        return verify() or manifest() or smoke()
    print(f"unknown command: {cmd!r}; supported: verify, smoke, manifest, all", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main())
