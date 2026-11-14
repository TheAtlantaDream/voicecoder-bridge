"""Drop-in whisper.cpp-compatible /inference server backed by Parakeet TDT."""

from __future__ import annotations

import os
import tempfile
from contextlib import asynccontextmanager
from pathlib import Path

import numpy as np
import sherpa_onnx
import soundfile as sf
from fastapi import FastAPI, File, Form, UploadFile
from fastapi.responses import JSONResponse, PlainTextResponse

MODEL_ROOT = Path(os.environ.get("WHISPER_DOWNLOAD_ROOT", "/models"))
MODEL_NAME = os.environ.get(
    "PARAKEET_MODEL",
    "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8",
)
CPU_THREADS = int(os.environ.get("WHISPER_CPU_THREADS", "8"))
PROVIDER = os.environ.get("PARAKEET_PROVIDER", "cpu")

recognizer: sherpa_onnx.OfflineRecognizer | None = None


def model_dir() -> Path:
    return MODEL_ROOT / MODEL_NAME


def load_recognizer() -> sherpa_onnx.OfflineRecognizer:
    d = model_dir()
    encoder = d / "encoder.int8.onnx"
    decoder = d / "decoder.int8.onnx"
    joiner = d / "joiner.int8.onnx"
    tokens = d / "tokens.txt"
    for p in (encoder, decoder, joiner, tokens):
        if not p.is_file():
            raise FileNotFoundError(
                f"missing {p}; run model-download (./model-download.sh) first"
            )
    return sherpa_onnx.OfflineRecognizer.from_transducer(
        encoder=str(encoder),
        decoder=str(decoder),
        joiner=str(joiner),
        tokens=str(tokens),
        num_threads=CPU_THREADS,
        provider=PROVIDER,
        model_type="nemo_transducer",
        decoding_method="greedy_search",
    )


def to_mono_float32(path: str) -> tuple[np.ndarray, int]:
    audio, sample_rate = sf.read(path, dtype="float32", always_2d=True)
    mono = audio[:, 0]
    if sample_rate == 16000:
        return mono, sample_rate
    # Linear resample to 16 kHz (bridge always sends 16 kHz WAV).
    n = int(round(len(mono) * 16000 / sample_rate))
    if n <= 0:
        return np.zeros(0, dtype=np.float32), 16000
    x_old = np.linspace(0.0, 1.0, num=len(mono), endpoint=False)
    x_new = np.linspace(0.0, 1.0, num=n, endpoint=False)
    return np.interp(x_new, x_old, mono).astype(np.float32), 16000


@asynccontextmanager
async def lifespan(_app: FastAPI):
    global recognizer
    recognizer = load_recognizer()
    yield


app = FastAPI(title="voicecoder-parakeet", lifespan=lifespan)


@app.get("/")
def root():
    return {
        "status": "ok",
        "engine": "parakeet-tdt",
        "model": MODEL_NAME,
        "provider": PROVIDER,
    }


@app.post("/inference")
async def inference(
    file: UploadFile = File(...),
    response_format: str = Form("json"),
):
    """Match whisper-server's multipart /inference used by the bridge."""
    if recognizer is None:
        return JSONResponse({"error": "model not loaded"}, status_code=503)

    raw = await file.read()
    suffix = os.path.splitext(file.filename or "audio.wav")[1] or ".wav"
    with tempfile.NamedTemporaryFile(suffix=suffix, delete=False) as tmp:
        tmp.write(raw)
        path = tmp.name

    try:
        samples, sample_rate = to_mono_float32(path)
        stream = recognizer.create_stream()
        stream.accept_waveform(sample_rate, samples)
        recognizer.decode_stream(stream)
        text = (stream.result.text or "").strip()
    finally:
        os.unlink(path)

    if response_format == "text":
        return PlainTextResponse(text)
    return JSONResponse({"text": text})
