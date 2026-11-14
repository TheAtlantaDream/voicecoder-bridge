"""Pre-download the faster-whisper model into WHISPER_DOWNLOAD_ROOT."""

from __future__ import annotations

import os

from faster_whisper import WhisperModel

MODEL = os.environ.get("WHISPER_MODEL", "small.en")
DOWNLOAD_ROOT = os.environ.get("WHISPER_DOWNLOAD_ROOT", "/models")

print(f"downloading {MODEL} into {DOWNLOAD_ROOT} ...", flush=True)
WhisperModel(MODEL, device="cpu", compute_type="int8", download_root=DOWNLOAD_ROOT)
print("ok", flush=True)
