"""Download the Parakeet TDT sherpa-onnx package into WHISPER_DOWNLOAD_ROOT."""

from __future__ import annotations

import hashlib
import os
import shutil
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
URL = os.environ.get(
    "PARAKEET_MODEL_URL",
    f"https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/{MODEL_NAME}.tar.bz2",
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


def validate_model(dest: Path, *, check_hash: bool = False) -> str | None:
    """Return an error string if the model looks corrupt, else None."""
    spec = EXPECTED.get(MODEL_NAME)
    if spec is None:
        # Unknown package: only require the usual ONNX files to exist and be non-tiny.
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


def acquire_lock(lock: Path, timeout_s: float = 1800) -> bool:
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            fd = os.open(str(lock), os.O_CREAT | os.O_EXCL | os.O_WRONLY)
            os.write(fd, str(os.getpid()).encode())
            os.close(fd)
            return True
        except FileExistsError:
            # Stale lock older than timeout: steal it.
            try:
                age = time.time() - lock.stat().st_mtime
                if age > timeout_s:
                    lock.unlink(missing_ok=True)
                    continue
            except OSError:
                pass
            time.sleep(2)
    return False


def download_and_extract(dest: Path) -> None:
    MODEL_ROOT.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=MODEL_ROOT) as tmp:
        tmp_path = Path(tmp)
        archive = tmp_path / f"{MODEL_NAME}.tar.bz2"
        print(f"downloading {URL} ...", flush=True)
        urllib.request.urlretrieve(URL, archive)
        size = archive.stat().st_size
        if size < 100_000_000:
            raise SystemExit(f"download too small ({size} bytes): {archive}")
        print(f"extracting {archive} ({size} bytes) ...", flush=True)
        with tarfile.open(archive, "r:bz2") as tar:
            tar.extractall(tmp_path, filter="data")
        extracted = tmp_path / MODEL_NAME
        if not extracted.is_dir():
            # Some archives extract flat; accept either layout.
            raise SystemExit(f"expected directory {extracted} after extract")
        err = validate_model(extracted, check_hash=True)
        if err:
            raise SystemExit(f"downloaded model failed validation: {err}")
        if dest.exists():
            shutil.rmtree(dest)
        shutil.move(str(extracted), str(dest))
    print("ok", flush=True)


def main(*, force: bool | None = None) -> None:
    force = FORCE if force is None else force
    dest = MODEL_ROOT / MODEL_NAME
    lock = MODEL_ROOT / f".{MODEL_NAME}.lock"

    if not force:
        err = validate_model(dest) if dest.is_dir() else "missing model dir"
        if err is None:
            print(f"already present: {dest}", flush=True)
            return
        if dest.is_dir():
            print(f"corrupt model ({err}); re-downloading", flush=True)

    if not acquire_lock(lock):
        raise SystemExit(f"timed out waiting for download lock {lock}")
    try:
        # Another process may have finished while we waited.
        if not force and dest.is_dir() and validate_model(dest) is None:
            print(f"already present: {dest}", flush=True)
            return
        download_and_extract(dest)
    finally:
        lock.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
