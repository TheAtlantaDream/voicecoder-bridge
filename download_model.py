"""Download the Parakeet TDT sherpa-onnx package into WHISPER_DOWNLOAD_ROOT."""

from __future__ import annotations

import hashlib
import os
import shutil
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

MODEL_ROOT = Path(os.environ.get("WHISPER_DOWNLOAD_ROOT", "/models"))
MODEL_NAME = os.environ.get(
    "PARAKEET_MODEL",
    "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8",
)
URL = os.environ.get(
    "PARAKEET_MODEL_URL",
    f"https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/{MODEL_NAME}.tar.bz2",
)
# Hugging Face mirror — often more reliable from restricted networks.
HF_URL = os.environ.get(
    "PARAKEET_MODEL_HF_URL",
    f"https://huggingface.co/csukuangfj/{MODEL_NAME}/resolve/main/{MODEL_NAME}.tar.bz2",
)
FORCE = os.environ.get("PARAKEET_FORCE_DOWNLOAD", "").lower() in ("1", "true", "yes")

# Official sizes / digests for v2 int8 (k2-fsa/sherpa-onnx#2216).
EXPECTED: dict[str, dict[str, object]] = {
    "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8": {
        "files": {
            "encoder.int8.onnx": {
                "min_bytes": 600_000_000,
                "sha256": "a32b12d17bbbc309d0686fbbcc2987b5e9b8333a7da83fa6b089f0a2acd651ab",
            },
            "decoder.int8.onnx": {
                "min_bytes": 6_000_000,
                "sha256": "b6bb64963457237b900e496ee9994b59294526439fbcc1fecf705b31a15c6b4e",
            },
            "joiner.int8.onnx": {
                "min_bytes": 1_000_000,
                "sha256": "7946164367946e7f9f29a122407c3252b680dbae9a51343eb2488d057c3c43d2",
            },
            "tokens.txt": {"min_bytes": 1_000},
        }
    }
}


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def validate_model(dest: Path, *, check_hash: bool | None = None) -> str | None:
    """Return an error string if the model looks corrupt, else None.

    For known packages, SHA-256 is checked by default — size alone missed
    full-sized but truncated/corrupt encoder files.
    """
    spec = EXPECTED.get(MODEL_NAME)
    if check_hash is None:
        check_hash = spec is not None
    if spec is None:
        for name, minimum in (
            ("encoder.int8.onnx", 50_000_000),
            ("decoder.int8.onnx", 100_000),
            ("joiner.int8.onnx", 100_000),
            ("tokens.txt", 100),
        ):
            p = dest / name
            if not p.is_file():
                return f"missing {p}"
            if p.stat().st_size < minimum:
                return f"{p} too small ({p.stat().st_size} bytes)"
        return None

    for name, meta in spec["files"].items():  # type: ignore[union-attr]
        p = dest / name
        if not p.is_file():
            return f"missing {p}"
        size = p.stat().st_size
        minimum = int(meta["min_bytes"])  # type: ignore[index]
        if size < minimum:
            return f"{p.name} too small ({size} bytes; want >= {minimum})"
        digest = meta.get("sha256")  # type: ignore[union-attr]
        if check_hash and digest:
            got = sha256_file(p)
            if got != digest:
                return f"{p.name} sha256 mismatch (got {got}, want {digest})"
    return None


def describe_model(dest: Path) -> str:
    if not dest.is_dir():
        return f"{dest} (missing)"
    parts = []
    for p in sorted(dest.glob("*")):
        if p.is_file():
            parts.append(f"{p.name}={p.stat().st_size}")
    return ", ".join(parts) or f"{dest} (empty)"


def acquire_lock(lock: Path, timeout_s: float = 1800) -> bool:
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            fd = os.open(str(lock), os.O_CREAT | os.O_EXCL | os.O_WRONLY)
            os.write(fd, str(os.getpid()).encode())
            os.close(fd)
            return True
        except FileExistsError:
            try:
                age = time.time() - lock.stat().st_mtime
                if age > timeout_s:
                    lock.unlink(missing_ok=True)
                    continue
            except OSError:
                pass
            time.sleep(2)
    return False


def _fetch(url: str, dest: Path) -> None:
    print(f"downloading {url} ...", flush=True)
    req = urllib.request.Request(url, headers={"User-Agent": "voicecoder-bridge/1.0"})
    with urllib.request.urlopen(req, timeout=600) as resp, dest.open("wb") as out:
        shutil.copyfileobj(resp, out, length=16 * 1024 * 1024)


def download_and_extract(dest: Path) -> None:
    MODEL_ROOT.mkdir(parents=True, exist_ok=True)
    urls = [URL]
    if HF_URL and HF_URL not in urls:
        urls.append(HF_URL)

    last_err: Exception | None = None
    with tempfile.TemporaryDirectory(dir=MODEL_ROOT) as tmp:
        tmp_path = Path(tmp)
        archive = tmp_path / f"{MODEL_NAME}.tar.bz2"
        for url in urls:
            try:
                if archive.exists():
                    archive.unlink()
                _fetch(url, archive)
                size = archive.stat().st_size
                if size < 100_000_000:
                    raise RuntimeError(f"download too small ({size} bytes) from {url}")
                # Sanity: bz2 magic
                with archive.open("rb") as f:
                    magic = f.read(3)
                if magic != b"BZh":
                    raise RuntimeError(
                        f"not a bz2 archive from {url} (magic={magic!r}); got HTML/error page?"
                    )
                print(f"extracting {archive} ({size} bytes) ...", flush=True)
                with tarfile.open(archive, "r:bz2") as tar:
                    tar.extractall(tmp_path, filter="data")
                extracted = tmp_path / MODEL_NAME
                if not extracted.is_dir():
                    raise RuntimeError(f"expected directory {extracted} after extract")
                err = validate_model(extracted, check_hash=True)
                if err:
                    raise RuntimeError(f"downloaded model failed validation: {err}")
                if dest.exists():
                    shutil.rmtree(dest)
                shutil.move(str(extracted), str(dest))
                print(f"ok — {describe_model(dest)}", flush=True)
                return
            except (urllib.error.URLError, urllib.error.HTTPError, OSError, RuntimeError) as exc:
                last_err = exc
                print(f"download from {url} failed: {exc}", flush=True)
                continue
    raise SystemExit(f"all download mirrors failed: {last_err}")


def main(*, force: bool | None = None) -> None:
    force = FORCE if force is None else force
    dest = MODEL_ROOT / MODEL_NAME
    lock = MODEL_ROOT / f".{MODEL_NAME}.lock"

    print(f"model path: {dest} ({describe_model(dest)})", flush=True)

    if not force:
        err = validate_model(dest) if dest.is_dir() else "missing model dir"
        if err is None:
            print(f"already present and valid: {dest}", flush=True)
            return
        if dest.is_dir():
            print(f"corrupt model ({err}); re-downloading", flush=True)

    if not acquire_lock(lock):
        raise SystemExit(f"timed out waiting for download lock {lock}")
    try:
        if not force and dest.is_dir() and validate_model(dest) is None:
            print(f"already present and valid: {dest}", flush=True)
            return
        download_and_extract(dest)
    finally:
        lock.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
