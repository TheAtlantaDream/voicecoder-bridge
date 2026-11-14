"""Drop-in whisper.cpp-compatible /inference server backed by faster-whisper."""

from __future__ import annotations

import os
import tempfile
from contextlib import asynccontextmanager

from fastapi import FastAPI, File, Form, UploadFile
from fastapi.responses import JSONResponse, PlainTextResponse
from faster_whisper import WhisperModel

MODEL = os.environ.get("WHISPER_MODEL", "small.en")
DEVICE = os.environ.get("WHISPER_DEVICE", "cpu")
COMPUTE = os.environ.get("WHISPER_COMPUTE_TYPE", "int8")
DOWNLOAD_ROOT = os.environ.get("WHISPER_DOWNLOAD_ROOT", "/models")
CPU_THREADS = int(os.environ.get("WHISPER_CPU_THREADS", "8"))
BEAM_SIZE = int(os.environ.get("WHISPER_BEAM_SIZE", "1"))

model: WhisperModel | None = None


@asynccontextmanager
async def lifespan(_app: FastAPI):
    global model
    model = WhisperModel(
        MODEL,
        device=DEVICE,
        compute_type=COMPUTE,
        download_root=DOWNLOAD_ROOT,
        cpu_threads=CPU_THREADS,
    )
    yield


app = FastAPI(title="voicecoder-faster-whisper", lifespan=lifespan)


@app.get("/")
def root():
    return {"status": "ok", "engine": "faster-whisper", "model": MODEL}


@app.post("/inference")
async def inference(
    file: UploadFile = File(...),
    response_format: str = Form("json"),
):
    """Match whisper-server's multipart /inference used by the bridge."""
    if model is None:
        return JSONResponse({"error": "model not loaded"}, status_code=503)

    raw = await file.read()
    suffix = os.path.splitext(file.filename or "audio.wav")[1] or ".wav"
    with tempfile.NamedTemporaryFile(suffix=suffix, delete=False) as tmp:
        tmp.write(raw)
        path = tmp.name

    try:
        segments, _info = model.transcribe(
            path,
            language="en",
            beam_size=BEAM_SIZE,
            vad_filter=True,
            condition_on_previous_text=False,
        )
        parts = [s.text.strip() for s in segments if s.text and s.text.strip()]
        text = " ".join(parts)
    finally:
        os.unlink(path)

    if response_format == "text":
        return PlainTextResponse(text)
    return JSONResponse({"text": text})
