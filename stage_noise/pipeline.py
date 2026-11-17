"""End-to-end orchestration: trace, denoise, chunked render, master."""

from __future__ import annotations

import os
import signal
import time

import numpy as np
import soundfile as sf

from .errors import BusinessCode, StageNoiseError
from .generator import verify_wav_headers
from .manifest import Manifest
from .persistence.snapshot import SnapshotStore, canonical_digest
from .processing.denoise import (bandstop_kernel, content_band_loss,
                                 denoise_microphone)
from .processing.mastering import master
from .processing.tracing import trace_all


class PipelineInterrupted(Exception):
    """Raised after a graceful SIGINT once the current chunk is committed."""


class _CorruptSnapshot(Exception):
    """Internal: on-disk snapshot is unusable and must be discarded."""


def _read_mics(manifest: Manifest) -> dict:
    signals = {}
    for mic in manifest.mics:
        path = os.path.join(manifest.source_dir, mic.path)
        data, sr = sf.read(path, dtype="float64", always_2d=False)
        if sr != manifest.sample_rate:
            raise StageNoiseError(BusinessCode.SAMPLE_RATE_MISMATCH,
                                  "sample rate changed before render",
                                  mic=mic.name)
        signals[mic.name] = np.asarray(data, dtype=np.float64)
    return signals


def _write_processed(manifest: Manifest, original: dict,
                     processed: dict) -> None:
    for mic in manifest.mics:
        path = os.path.join(manifest.source_dir, mic.path)
        out = processed[mic.name].copy()
        untouched = np.isclose(out, original[mic.name], atol=0.0)
        out[untouched] = original[mic.name][untouched]
        sf.write(path, out, manifest.sample_rate, subtype=mic.subtype)


def chunk_ranges(total: int, chunk: int) -> list:
    return [(i, min(i + chunk, total))
            for i in range(0, total, chunk)]


def run_pipeline(manifest: Manifest, state_dir: str, outputs: dict,
                 interrupt_after_chunk: int | None = None) -> dict:
    verify_wav_headers(manifest)
    original = _read_mics(manifest)
    store = SnapshotStore(state_dir)

    trace_results = trace_all(original, manifest)
    trace_digest = canonical_digest(trace_results)
    manifest_digest = canonical_digest(manifest.raw)
    kernel = bandstop_kernel(manifest.noise_band_hz, manifest.sample_rate)
    denoise_params = {
        "filter_taps": int(len(kernel)),
        "noise_band_hz": list(manifest.noise_band_hz),
        "crossfade_ms": 30.0,
        "residual_threshold_dbfs": manifest.residual_threshold_dbfs,
        "min_attenuation_db": manifest.min_attenuation_db,
    }
    params_digest = canonical_digest(denoise_params)

    denoise_reports = []
    processed = {}
    for mic in manifest.mics:
        trace_entry = next(t for t in trace_results
                           if t["mic_name"] == mic.name)
        result = denoise_microphone(original[mic.name], kernel, trace_entry,
                                    manifest)
        processed[mic.name] = result.pop("processed")
        result["content_band"] = content_band_loss(
            original[mic.name], processed[mic.name], manifest)
        denoise_reports.append(result)

    total_samples = max(len(sig) for sig in original.values())
    ranges = chunk_ranges(total_samples, manifest.chunk_samples)

    snapshot = store.load()
    start_chunk = 0
    part_hashes = []
    interrupted = False
    if snapshot is not None:
        try:
            if (snapshot.get("manifest_digest") != manifest_digest
                    or snapshot.get("trace_digest") != trace_digest
                    or snapshot.get("denoise_params_digest") != params_digest):
                store.reset()
                raise StageNoiseError(
                    BusinessCode.E_RESUME_MISMATCH,
                    "snapshot does not match current manifest or tracing; "
                    "old progress discarded",
                    details={"snapshot_chunks":
                             snapshot.get("completed_chunks")})
            start_chunk = int(snapshot["completed_chunks"])
            part_hashes = list(snapshot["part_hashes"])
            try:
                store.verify_parts(part_hashes)
            except StageNoiseError as error:
                if error.code == BusinessCode.SNAPSHOT_CORRUPT:
                    raise _CorruptSnapshot(str(error))
                raise
            if start_chunk > len(ranges):
                raise _CorruptSnapshot("snapshot chunk count exceeds input")
        except StageNoiseError:
            raise
        except (_CorruptSnapshot, KeyError, ValueError, TypeError, OSError):
            store.reset()
            start_chunk = 0
            part_hashes = []
            snapshot = None

    sigint_received = {"flag": False}

    def _stereo_chunk(seg_start: int, seg_end: int) -> np.ndarray:
        # Downmix is computed per chunk so a resumed run and a one-shot run
        # assemble exactly the same floating-point values.
        chunk = np.zeros((seg_end - seg_start, 2), dtype=np.float64)
        for mic in manifest.mics:
            signal = processed[mic.name]
            if seg_start >= len(signal):
                continue
            upper = min(seg_end, len(signal))
            view = signal[seg_start:upper]
            chunk[:upper - seg_start, 0] += mic.downmix[0] * view
            chunk[:upper - seg_start, 1] += mic.downmix[1] * view
        return chunk

    def _handler(signum, frame):
        sigint_received["flag"] = True

    previous_handler = signal.signal(signal.SIGINT, _handler)
    try:
        for index in range(start_chunk, len(ranges)):
            seg_start, seg_end = ranges[index]
            part = _stereo_chunk(seg_start, seg_end)
            part_hash = store.write_part(index, part)
            part_hashes.append(part_hash)
            store.commit({
                "version": 1,
                "manifest_digest": manifest_digest,
                "trace_digest": trace_digest,
                "denoise_params_digest": params_digest,
                "denoise_params": denoise_params,
                "completed_chunks": index + 1,
                "total_chunks": len(ranges),
                "chunk_samples": manifest.chunk_samples,
                "part_hashes": part_hashes,
                "trace_results": trace_results,
            })
            chunk_delay = os.environ.get("STAGE_NOISE_CHUNK_DELAY_MS")
            if chunk_delay:
                time.sleep(float(chunk_delay) / 1000.0)
            if sigint_received["flag"] or (interrupt_after_chunk is not None
                                           and index >= interrupt_after_chunk):
                interrupted = True
                break
    finally:
        signal.signal(signal.SIGINT, previous_handler)

    if interrupted:
        raise PipelineInterrupted(store.path)

    assembled = np.vstack([
        store.read_part(index) for index in range(len(ranges))])
    if assembled.shape != (total_samples, 2):
        raise StageNoiseError(BusinessCode.SNAPSHOT_CORRUPT,
                              "assembled master has unexpected shape")

    master_result = master(assembled, manifest)
    master_audio = master_result.pop("master")
    if len(master_audio) != total_samples:
        raise StageNoiseError(BusinessCode.INVALID_MANIFEST,
                              "limiter changed master length")

    sf.write(outputs["master"], master_audio, manifest.sample_rate,
             subtype="PCM_24")
    _write_processed(manifest, original, processed)
    store.cleanup()

    return {
        "trace_results": trace_results,
        "denoise_reports": denoise_reports,
        "master_report": master_result,
        "total_samples": total_samples,
        "chunk_count": len(ranges),
        "master_path": outputs["master"],
        "processed_paths": {mic.name: os.path.join(manifest.source_dir,
                                                   mic.path)
                            for mic in manifest.mics},
    }
