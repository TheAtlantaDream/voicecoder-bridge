"""Download the Parakeet TDT sherpa-onnx package into WHISPER_DOWNLOAD_ROOT."""

from __future__ import annotations

import os
import tarfile
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


def main() -> None:
    dest = MODEL_ROOT / MODEL_NAME
    marker = dest / "encoder.int8.onnx"
    if marker.is_file():
        print(f"already present: {dest}", flush=True)
        return

    MODEL_ROOT.mkdir(parents=True, exist_ok=True)
    archive = MODEL_ROOT / f"{MODEL_NAME}.tar.bz2"
    print(f"downloading {URL} ...", flush=True)
    urllib.request.urlretrieve(URL, archive)
    print(f"extracting {archive} ...", flush=True)
    with tarfile.open(archive, "r:bz2") as tar:
        tar.extractall(MODEL_ROOT, filter="data")
    archive.unlink(missing_ok=True)
    if not marker.is_file():
        raise SystemExit(f"download finished but {marker} is missing")
    print("ok", flush=True)


if __name__ == "__main__":
    main()
