#!/usr/bin/env python3
"""Build a deterministic Sub2API `.s2plugin` archive.

The script is deliberately dependency-free.  It packages one or more already
built runtime binaries together with the static UI, writes SHA-256 entries to
`manifest.json` and optionally includes an externally-produced Ed25519
`signature.json`.
"""

from __future__ import annotations

import argparse
import base64
import binascii
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import zipfile


RUNTIME_RE = re.compile(r"^[a-z0-9]+-[a-z0-9]+$")
SAFE_PATH_RE = re.compile(r"^(?!/)(?!.*(?:^|/)\.\.(?:/|$))(?!.*\\).+$")
GOOS_VALUES = {"linux", "windows", "darwin"}
GOARCH_VALUES = {"amd64", "arm64"}


class PackageError(ValueError):
    """An input or package-layout error suitable for a CLI diagnostic."""


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--manifest-source", type=Path)
    parser.add_argument(
        "--binary",
        type=Path,
        action="append",
        help="runtime binary path; pair with --goos and --goarch",
    )
    parser.add_argument(
        "--runtime-binary",
        action="append",
        metavar="RUNTIME=PATH",
        help="explicit runtime/binary pair, e.g. windows-amd64=dist/plugin.exe",
    )
    parser.add_argument("--goos", help="GOOS for every --binary entry")
    parser.add_argument("--goarch", help="GOARCH for every --binary entry")
    parser.add_argument("--signature", type=Path, help="optional signature.json")
    parser.add_argument("--allow-unsigned", action="store_true")
    parser.add_argument("--output", type=Path, required=True)
    return parser.parse_args()


def resolve_path(root: Path, value: Path) -> Path:
    return (value if value.is_absolute() else root / value).resolve()


def require_safe_path(value: str) -> str:
    if not isinstance(value, str) or not value or not SAFE_PATH_RE.fullmatch(value):
        raise PackageError(f"unsafe package path: {value!r}")
    return value


def read_json(path: Path) -> dict:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise PackageError(f"cannot read JSON file {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise PackageError(f"JSON root must be an object: {path}")
    return value


def validate_source_manifest(manifest: dict) -> None:
    if manifest.get("schema_version") != 1:
        raise PackageError("manifest.source.json must have schema_version=1")
    if not isinstance(manifest.get("id"), str) or not manifest["id"]:
        raise PackageError("manifest.source.json must contain a non-empty id")
    if not isinstance(manifest.get("version"), str) or not manifest["version"]:
        raise PackageError("manifest.source.json must contain a version")
    runtimes = manifest.get("runtimes")
    if not isinstance(runtimes, dict) or not runtimes:
        raise PackageError("manifest.source.json must declare at least one runtime")
    for runtime, entry in runtimes.items():
        if not isinstance(runtime, str) or not RUNTIME_RE.fullmatch(runtime):
            raise PackageError(f"invalid runtime key in manifest.source.json: {runtime!r}")
        if not isinstance(entry, dict) or not isinstance(entry.get("path"), str):
            raise PackageError(f"runtime {runtime!r} must declare a path")
        require_safe_path(entry["path"])
    ui = manifest.get("ui")
    if not isinstance(ui, dict) or not isinstance(ui.get("entrypoint"), str):
        raise PackageError("manifest.source.json must declare ui.entrypoint")
    require_safe_path(ui["entrypoint"])


def parse_runtime_binaries(args: argparse.Namespace, root: Path, source: dict) -> dict[str, Path]:
    if args.binary and args.runtime_binary:
        raise PackageError("use either --binary or --runtime-binary, not both")
    result: dict[str, Path] = {}
    declared = source["runtimes"]
    if args.binary:
        if not args.goos or not args.goarch:
            raise PackageError("--binary requires both --goos and --goarch")
        if args.goos not in GOOS_VALUES or args.goarch not in GOARCH_VALUES:
            raise PackageError("supported targets are linux/windows/darwin and amd64/arm64")
        runtime = f"{args.goos}-{args.goarch}"
        if len(args.binary) != 1:
            raise PackageError("one --binary is accepted per invocation; use --runtime-binary for multiple targets")
        result[runtime] = resolve_path(root, args.binary[0])
    elif args.runtime_binary:
        for item in args.runtime_binary:
            if "=" not in item:
                raise PackageError(f"--runtime-binary must be RUNTIME=PATH: {item!r}")
            runtime, raw_path = item.split("=", 1)
            runtime = runtime.strip()
            if not RUNTIME_RE.fullmatch(runtime):
                raise PackageError(f"invalid runtime key: {runtime!r}")
            if runtime in result:
                raise PackageError(f"duplicate runtime binary: {runtime}")
            result[runtime] = resolve_path(root, Path(raw_path))
    else:
        raise PackageError("at least one --binary or --runtime-binary is required")

    for runtime, binary in result.items():
        if runtime not in declared:
            raise PackageError(f"runtime {runtime!r} is not declared in manifest.source.json")
        if not binary.is_file() or binary.is_symlink():
            raise PackageError(f"runtime binary does not exist or is a symlink: {binary}")
    return result


def collect_ui_files(root: Path, source: dict) -> dict[str, Path]:
    ui_root = (root / "ui").resolve()
    if not ui_root.is_dir() or ui_root.is_symlink():
        raise PackageError(f"UI directory is missing or is a symlink: {ui_root}")
    files: dict[str, Path] = {}
    for path in sorted(ui_root.rglob("*")):
        if path.is_symlink():
            raise PackageError(f"UI must not contain symlinks: {path}")
        if not path.is_file():
            continue
        package_path = path.relative_to(root).as_posix()
        require_safe_path(package_path)
        files[package_path] = path
    entrypoint = source["ui"]["entrypoint"]
    if entrypoint not in files:
        raise PackageError(f"UI entrypoint is not present: {entrypoint}")
    if not files:
        raise PackageError("UI directory contains no files")
    return files


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(chunk)
    except OSError as exc:
        raise PackageError(f"cannot hash {path}: {exc}") from exc
    return digest.hexdigest()


def manifest_bytes(manifest: dict) -> bytes:
    return (json.dumps(manifest, ensure_ascii=False, indent=2) + "\n").encode("utf-8")


def load_signature(path: Path) -> bytes:
    signature = read_json(path)
    if set(signature) != {"algorithm", "key_id", "signature"}:
        raise PackageError("signature.json must contain exactly algorithm, key_id, and signature")
    if signature["algorithm"] != "ed25519":
        raise PackageError("signature.json algorithm must be ed25519")
    if not isinstance(signature["key_id"], str) or not signature["key_id"].strip():
        raise PackageError("signature.json key_id must be a non-empty string")
    encoded = signature["signature"]
    if not isinstance(encoded, str):
        raise PackageError("signature.json signature must be Base64")
    try:
        raw = base64.b64decode(encoded, validate=True)
    except (ValueError, binascii.Error) as exc:
        raise PackageError(f"signature.json signature is not valid Base64: {exc}") from exc
    if len(raw) != 64:
        raise PackageError("signature.json signature must decode to 64 bytes")
    return manifest_bytes(signature)


def build_manifest(source: dict, runtime_files: dict[str, Path], ui_files: dict[str, Path]) -> tuple[dict, dict[str, Path]]:
    manifest = json.loads(json.dumps(source))
    manifest["runtimes"] = {
        runtime: {"path": source["runtimes"][runtime]["path"]}
        for runtime in sorted(runtime_files)
    }
    package_files: dict[str, Path] = {}
    for runtime in sorted(runtime_files):
        package_path = manifest["runtimes"][runtime]["path"]
        require_safe_path(package_path)
        if package_path in package_files:
            raise PackageError(f"duplicate package path: {package_path}")
        package_files[package_path] = runtime_files[runtime]
    for package_path, local_path in sorted(ui_files.items()):
        if package_path in package_files:
            raise PackageError(f"duplicate package path: {package_path}")
        package_files[package_path] = local_path
    manifest["files"] = {
        package_path: sha256_file(local_path)
        for package_path, local_path in sorted(package_files.items())
    }
    if len(manifest["files"]) < 2:
        raise PackageError("manifest.files must contain a runtime and a UI file")
    return manifest, package_files


def zip_info(name: str, executable: bool) -> zipfile.ZipInfo:
    info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
    info.compress_type = zipfile.ZIP_DEFLATED
    info.create_system = 3
    info.external_attr = (0o755 if executable else 0o644) << 16
    return info


def write_package(output: Path, manifest: bytes, files: dict[str, Path], signature: bytes | None) -> None:
    output.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{output.name}.", suffix=".tmp", dir=output.parent)
    os.close(descriptor)
    temporary = Path(temporary_name)
    try:
        with zipfile.ZipFile(temporary, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
            archive.writestr(zip_info("manifest.json", False), manifest)
            if signature is not None:
                archive.writestr(zip_info("signature.json", False), signature)
            for package_path in sorted(files):
                archive.writestr(
                    zip_info(package_path, package_path.startswith("runtimes/")),
                    files[package_path].read_bytes(),
                )
        os.replace(temporary, output)
    except (OSError, zipfile.BadZipFile) as exc:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise PackageError(f"cannot write package {output}: {exc}") from exc


def package(args: argparse.Namespace) -> Path:
    root = args.root.resolve()
    source_path = resolve_path(root, args.manifest_source or Path("manifest.source.json"))
    source = read_json(source_path)
    validate_source_manifest(source)
    runtime_files = parse_runtime_binaries(args, root, source)
    ui_files = collect_ui_files(root, source)
    manifest, files = build_manifest(source, runtime_files, ui_files)
    signature: bytes | None = None
    if args.signature:
        signature_path = resolve_path(root, args.signature)
        if not signature_path.is_file() or signature_path.is_symlink():
            raise PackageError(f"signature file does not exist or is a symlink: {signature_path}")
        signature = load_signature(signature_path)
    elif not args.allow_unsigned:
        raise PackageError("production packages require --signature; use --allow-unsigned for development")
    output = resolve_path(root, args.output)
    write_package(output, manifest_bytes(manifest), files, signature)
    return output


def main() -> int:
    try:
        output = package(parse_args())
    except PackageError as exc:
        print(f"package error: {exc}", file=sys.stderr)
        return 2
    print(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
