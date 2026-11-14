"""Drop-in whisper.cpp-compatible /inference server backed by Parakeet TDT."""

from __future__ import annotations

import logging
import os
import tempfile
import threading
from contextlib import asynccontextmanager
from pathlib import Path

import numpy as np
import soundfile as sf
from fastapi import FastAPI, File, Form, UploadFile
from fastapi.responses import JSONResponse, PlainTextResponse

MODEL_ROOT = Path(os.environ.get("WHISPER_DOWNLOAD_ROOT", "/models"))
MODEL_NAME = os.environ.get(
    "PARAKEET_MODEL",
    "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8",
)
CPU_THREADS = int(os.environ.get("WHISPER_CPU_THREADS", "4"))
PROVIDER = os.environ.get("PARAKEET_PROVIDER", "cpu")

log = logging.getLogger("voicecoder-parakeet")
logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")

recognizer = None
load_error: str | None = None
ready = threading.Event()


def model_dir() -> Path:
    return MODEL_ROOT / MODEL_NAME


def ensure_model(*, force: bool = False) -> None:
    """Download Parakeet if missing or corrupt (truncated downloads break ONNX)."""
    from download_model import describe_model
    from download_model import main as download_main
    from download_model import validate_model

    dest = model_dir()
    log.info("model files: %s", describe_model(dest))
    err = None if dest.is_dir() else "missing model dir"
    if err is None:
        err = validate_model(dest, check_hash=True)
    if force or err is not None:
        log.info("model needs download (%s); fetching %s", err or "forced", MODEL_NAME)
        download_main(force=True)


def _open_recognizer():
    import sherpa_onnx

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
        log.info("using %s (%d bytes)", p.name, p.stat().st_size)
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


def load_recognizer():
    ensure_model()
    try:
        return _open_recognizer()
    except Exception as exc:  # noqa: BLE001
        msg = str(exc).lower()
        if "protobuf" in msg or "parse" in msg or "load model" in msg:
            log.warning("onnx load failed (%s); wiping and re-downloading once", exc)
            ensure_model(force=True)
            return _open_recognizer()
        raise


_load_lock = threading.Lock()


def load_in_background() -> None:
    global recognizer, load_error
    with _load_lock:
        try:
            log.info("loading %s (%s threads)", MODEL_NAME, CPU_THREADS)
            recognizer = load_recognizer()
            load_error = None
            log.info("model ready")
        except Exception as exc:  # noqa: BLE001 — surface any load failure in /ready
            recognizer = None
            load_error = f"{type(exc).__name__}: {exc}"
            log.exception("model load failed: %s", load_error)
        finally:
            ready.set()


def to_mono_float32(path: str) -> tuple[np.ndarray, int]:
    audio, sample_rate = sf.read(path, dtype="float32", always_2d=True)
    mono = audio[:, 0]
    if sample_rate == 16000:
        return mono, sample_rate
    n = int(round(len(mono) * 16000 / sample_rate))
    if n <= 0:
        return np.zeros(0, dtype=np.float32), 16000
    x_old = np.linspace(0.0, 1.0, num=len(mono), endpoint=False)
    x_new = np.linspace(0.0, 1.0, num=n, endpoint=False)
    return np.interp(x_new, x_old, mono).astype(np.float32), 16000


@asynccontextmanager
async def lifespan(_app: FastAPI):
    # Serve health immediately; model load can take minutes on first boot.
    threading.Thread(target=load_in_background, name="parakeet-load", daemon=True).start()
    yield


app = FastAPI(title="voicecoder-parakeet", lifespan=lifespan)


@app.get("/healthz")
def healthz():
    """Liveness: process is up (used by Docker healthcheck)."""
    return {"status": "ok"}


@app.get("/")
@app.get("/ready")
def root():
    if recognizer is None:
        body = {
            "status": "starting" if not ready.is_set() else "error",
            "engine": "parakeet-tdt",
            "model": MODEL_NAME,
            "provider": PROVIDER,
            "error": load_error,
        }
        return JSONResponse(body, status_code=503)
    return {
        "status": "ok",
        "engine": "parakeet-tdt",
        "model": MODEL_NAME,
        "provider": PROVIDER,
    }


@app.post("/repair")
def repair():
    """Wipe/re-download the model and reload. For corrupt shared volumes."""
    global recognizer, load_error
    ready.clear()
    with _load_lock:
        recognizer = None
        load_error = None
        try:
            ensure_model(force=True)
            recognizer = _open_recognizer()
            load_error = None
            return {"status": "ok", "model": MODEL_NAME}
        except Exception as exc:  # noqa: BLE001
            load_error = f"{type(exc).__name__}: {exc}"
            log.exception("repair failed: %s", load_error)
            return JSONResponse(
                {"status": "error", "error": load_error},
                status_code=500,
            )
        finally:
            ready.set()


@app.post("/inference")
async def inference(
    file: UploadFile = File(...),
    response_format: str = Form("json"),
):
    """Match whisper-server's multipart /inference used by the bridge."""
    if recognizer is None:
        return JSONResponse(
            {"error": load_error or "model not loaded yet"},
            status_code=503,
        )

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
