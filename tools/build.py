#!/usr/bin/env python3
"""Build one Sub2API runtime and package it as a `.s2plugin` archive."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import subprocess
import sys


GOOS_VALUES = {"linux", "windows", "darwin"}
GOARCH_VALUES = {"amd64", "arm64"}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--go", default="go", help="Go executable")
    parser.add_argument("--goos", required=True, choices=sorted(GOOS_VALUES))
    parser.add_argument("--goarch", required=True, choices=sorted(GOARCH_VALUES))
    parser.add_argument("--binary", type=Path, help="runtime binary output path")
    parser.add_argument("--output", type=Path, help=".s2plugin output path")
    parser.add_argument("--signature", type=Path, help="optional signature.json")
    parser.add_argument("--allow-unsigned", action="store_true")
    return parser.parse_args()


def resolve_path(root: Path, value: Path) -> Path:
    return (value if value.is_absolute() else root / value).resolve()


def main() -> int:
    args = parse_args()
    root = args.root.resolve()
    runtime = f"{args.goos}-{args.goarch}"
    executable = "plugin.exe" if args.goos == "windows" else "plugin"
    binary = resolve_path(root, args.binary or Path("dist") / "runtimes" / runtime / executable)
    output = resolve_path(root, args.output or Path("dist") / f"oai-basispoints-{runtime}.s2plugin")
    binary.parent.mkdir(parents=True, exist_ok=True)

    environment = os.environ.copy()
    environment["GOOS"] = args.goos
    environment["GOARCH"] = args.goarch
    environment["CGO_ENABLED"] = "0"
    command = [
        args.go,
        "build",
        "-trimpath",
        "-buildvcs=false",
        "-ldflags",
        "-s -w",
        "-o",
        str(binary),
        "./cmd/oai-basispoints",
    ]
    print("building", runtime)
    subprocess.run(command, cwd=root, env=environment, check=True)

    package_command = [
        sys.executable,
        str(root / "tools" / "package.py"),
        "--root",
        str(root),
        "--runtime-binary",
        f"{runtime}={binary}",
        "--output",
        str(output),
    ]
    if args.signature:
        package_command.extend(["--signature", str(resolve_path(root, args.signature))])
    if args.allow_unsigned:
        package_command.append("--allow-unsigned")
    subprocess.run(package_command, cwd=root, check=True)
    print(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
