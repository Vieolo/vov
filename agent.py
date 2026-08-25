#!/usr/bin/env python3
"""Deterministic verification for the vov framework and its examples.

Usage:
    python3 agent.py verify   # build, vet, and test every module

Per the repo rules, run this instead of re-running the go commands by hand.
Each module is checked independently because `go ... ./...` never crosses a
module boundary, so a break in one module stays invisible to the others.
"""
import subprocess
import sys
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


def main() -> int:
    args = sys.argv[1:]
    if not args or args[0] == "verify":
        return verify()
    print(f"unknown command: {args[0]!r}; supported: verify", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main())
