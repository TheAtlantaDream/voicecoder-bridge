"""Download the Parakeet TDT model into WHISPER_DOWNLOAD_ROOT.

Prefers per-file Hugging Face downloads (more reliable than a huge GitHub
tarball from Coder networks). Only marks the model complete after SHA-256
checks so a killed mid-download never gets reused by the next workspace.
"""

from __future__ import annotations

import hashlib
import os
import shutil
import subprocess
import tarfile
import tempfile
import time
import urllib.request
from pathlib import Path

MODEL_ROOT = Path(os.environ.get("WHISPER_DOWNLOAD_ROOT", "/models"))
MODEL_NAME = os.environ.get(
    "PARAKEET_MODEL",
    "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8",
)
TAR_URL = os.environ.get(
    "PARAKEET_MODEL_URL",
    f"https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/{MODEL_NAME}.tar.bz2",
)
HF_BASE = os.environ.get(
    "PARAKEET_HF_BASE",
    f"https://huggingface.co/csukuangfj/{MODEL_NAME}/resolve/main",
)
FORCE = os.environ.get("PARAKEET_FORCE_DOWNLOAD", "").lower() in ("1", "true", "yes")
COMPLETE_NAME = ".voicecoder-complete"

# Byte sizes from Hugging Face LFS. Accept either HF or GitHub-release digests
# (same model name, occasionally different packaging hashes).
FILES: dict[str, dict[str, object]] = {
    "encoder.int8.onnx": {
        "bytes": 652_282_309,
        "sha256": (
            "92a10bd9c23936ea0566c29f963bc57c2a5df4261b6a8921c39abea66557ca8b",  # HF
            "a32b12d17bbbc309d0686fbbcc2987b5e9b8333a7da83fa6b089f0a2acd651ab",  # GH
        ),
    },
    "decoder.int8.onnx": {
        "bytes": 7_257_753,
        "sha256": (
            "4d097bae3bac5c96018d5293ba39f1b657296102b142affda40378f38b020f41",  # HF
            "b6bb64963457237b900e496ee9994b59294526439fbcc1fecf705b31a15c6b4e",  # GH
        ),
    },
    "joiner.int8.onnx": {
        "bytes": 1_739_080,
        "sha256": (
            "77a29f3e5ac7d37074e0af465391b80f314015d5b97f0f90c6c7767f1d56f428",  # HF
            "7946164367946e7f9f29a122407c3252b680dbae9a51343eb2488d057c3c43d2",  # GH
        ),
    },
    "tokens.txt": {
        "bytes": 9_384,
        "sha256": (),  # small text file; size is enough
    },
}


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def describe_model(dest: Path) -> str:
    if not dest.is_dir():
        return f"{dest} (missing)"
    parts = []
    for p in sorted(dest.iterdir()):
        if p.is_file():
            parts.append(f"{p.name}={p.stat().st_size}")
    return ", ".join(parts) or f"{dest} (empty)"


def validate_model(dest: Path, *, check_hash: bool = True) -> str | None:
    if not dest.is_dir():
        return "missing model dir"
    complete = dest / COMPLETE_NAME
    if not complete.is_file():
        return "missing completion marker (download was interrupted)"
    for name, meta in FILES.items():
        p = dest / name
        if not p.is_file():
            return f"missing {name}"
        size = p.stat().st_size
        want = int(meta["bytes"])
        # Allow ±1% for tokens; exact for ONNX.
        if name.endswith(".onnx"):
            if size != want:
                return f"{name} size {size}, want {want}"
        elif abs(size - want) > max(1024, want // 50):
            return f"{name} size {size}, want ~{want}"
        digests = tuple(meta.get("sha256") or ())
        if check_hash and digests:
            got = sha256_file(p)
            if got not in digests:
                return f"{name} sha256 mismatch ({got[:12]}…)"
    return None


def acquire_lock(lock: Path, timeout_s: float = 1800) -> bool:
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            fd = os.open(str(lock), os.O_CREAT | os.O_EXCL | os.O_WRONLY)
            os.write(fd, f"{os.getpid()}\n".encode())
            os.close(fd)
            return True
        except FileExistsError:
            try:
                if time.time() - lock.stat().st_mtime > timeout_s:
                    lock.unlink(missing_ok=True)
                    continue
            except OSError:
                pass
            time.sleep(2)
    return False


def _curl(url: str, dest: Path) -> None:
    dest.parent.mkdir(parents=True, exist_ok=True)
    partial = dest.with_suffix(dest.suffix + ".partial")
    cmd = [
        "curl",
        "-fL",
        "--retry",
        "5",
        "--retry-delay",
        "2",
        "--connect-timeout",
        "30",
        "--max-time",
        "1800",
        "-o",
        str(partial),
        url,
    ]
    # Resume if a previous partial exists.
    if partial.exists() and partial.stat().st_size > 0:
        cmd.insert(1, "-C")
        cmd.insert(2, "-")
    print(f"curl {url}", flush=True)
    subprocess.run(cmd, check=True)
    partial.replace(dest)


def _urllib_fetch(url: str, dest: Path) -> None:
    print(f"urllib {url}", flush=True)
    req = urllib.request.Request(url, headers={"User-Agent": "voicecoder-bridge/1.0"})
    with urllib.request.urlopen(req, timeout=600) as resp, dest.open("wb") as out:
        shutil.copyfileobj(resp, out, length=16 * 1024 * 1024)


def fetch(url: str, dest: Path) -> None:
    if shutil.which("curl"):
        _curl(url, dest)
    else:
        _urllib_fetch(url, dest)


def write_complete_marker(dest: Path) -> None:
    lines = []
    for name in FILES:
        p = dest / name
        lines.append(f"{name} {p.stat().st_size} {sha256_file(p)}")
    (dest / COMPLETE_NAME).write_text("\n".join(lines) + "\n")


def download_from_hf(dest: Path) -> None:
    staging = Path(tempfile.mkdtemp(prefix=f"{MODEL_NAME}.", dir=MODEL_ROOT))
    try:
        for name, meta in FILES.items():
            out = staging / name
            fetch(f"{HF_BASE}/{name}", out)
            want = int(meta["bytes"])
            got = out.stat().st_size
            if name.endswith(".onnx") and got != want:
                raise RuntimeError(f"{name} size {got}, want {want} (truncated HF download)")
            digests = tuple(meta.get("sha256") or ())
            if digests:
                actual = sha256_file(out)
                if actual not in digests:
                    raise RuntimeError(f"{name} sha256 mismatch after HF download ({actual})")
            print(f"ok {name} ({got} bytes)", flush=True)
        write_complete_marker(staging)
        if dest.exists():
            shutil.rmtree(dest)
        staging.rename(dest)
    except Exception:
        shutil.rmtree(staging, ignore_errors=True)
        raise


def download_from_tarball(dest: Path) -> None:
    staging_root = Path(tempfile.mkdtemp(prefix=f"{MODEL_NAME}-tar.", dir=MODEL_ROOT))
    try:
        archive = staging_root / f"{MODEL_NAME}.tar.bz2"
        fetch(TAR_URL, archive)
        size = archive.stat().st_size
        if size < 100_000_000:
            raise RuntimeError(f"tarball too small ({size})")
        with archive.open("rb") as f:
            if f.read(3) != b"BZh":
                raise RuntimeError("tarball is not bz2 (got an error page?)")
        print(f"extracting {archive} ({size} bytes)", flush=True)
        with tarfile.open(archive, "r:bz2") as tar:
            tar.extractall(staging_root, filter="data")
        extracted = staging_root / MODEL_NAME
        if not extracted.is_dir():
            raise RuntimeError(f"missing {extracted} in tarball")
        for name, meta in FILES.items():
            p = extracted / name
            if not p.is_file():
                raise RuntimeError(f"tarball missing {name}")
            digests = tuple(meta.get("sha256") or ())
            if digests and sha256_file(p) not in digests:
                raise RuntimeError(f"tarball {name} sha256 mismatch")
        write_complete_marker(extracted)
        err = validate_model(extracted)
        if err:
            raise RuntimeError(err)
        if dest.exists():
            shutil.rmtree(dest)
        extracted.rename(dest)
    finally:
        shutil.rmtree(staging_root, ignore_errors=True)


def download_and_install(dest: Path) -> None:
    MODEL_ROOT.mkdir(parents=True, exist_ok=True)
    errors: list[str] = []
    try:
        print("trying Hugging Face per-file download ...", flush=True)
        download_from_hf(dest)
        print(f"ok via HF — {describe_model(dest)}", flush=True)
        return
    except Exception as exc:  # noqa: BLE001
        errors.append(f"HF: {exc}")
        print(f"HF download failed: {exc}", flush=True)
    try:
        print("trying GitHub tarball ...", flush=True)
        download_from_tarball(dest)
        print(f"ok via tarball — {describe_model(dest)}", flush=True)
        return
    except Exception as exc:  # noqa: BLE001
        errors.append(f"tar: {exc}")
        print(f"tarball download failed: {exc}", flush=True)
    raise SystemExit("model download failed: " + " | ".join(errors))


def main(*, force: bool | None = None) -> None:
    force = FORCE if force is None else force
    dest = MODEL_ROOT / MODEL_NAME
    lock = MODEL_ROOT / f".{MODEL_NAME}.lock"

    print(f"model path: {dest}", flush=True)
    print(f"current: {describe_model(dest)}", flush=True)

    if not force:
        err = validate_model(dest)
        if err is None:
            print("already present and valid", flush=True)
            return
        print(f"need download ({err})", flush=True)

    if not acquire_lock(lock):
        raise SystemExit(f"timed out waiting for {lock}")
    try:
        if not force and validate_model(dest) is None:
            print("already present and valid", flush=True)
            return
        # Drop any incomplete tree before installing.
        if dest.exists():
            print(f"removing incomplete/corrupt tree {dest}", flush=True)
            shutil.rmtree(dest)
        download_and_install(dest)
    finally:
        lock.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
