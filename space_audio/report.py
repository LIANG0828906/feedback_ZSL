"""Stable, deterministic spatial audio report and master comparison."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

import numpy as np

from .errors import MASTER_MISMATCH, BusinessError
from .renderer import RenderResult
from .wavio import read_wav


def sha256_file(path: str | Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _rounded(value: float, digits: int = 9) -> float:
    return float(np.round(float(value), digits))


def build_report(scene: dict, result: RenderResult) -> dict:
    table = result.frame_table
    frames = []
    for frame in range(table["frame_count"]):
        entry = {
            "frame": frame,
            "time_s": _rounded(table["frame_times"][frame], 6),
            "listener_position": [_rounded(v, 6) for v in table["listener_positions"][frame]],
            "listener_yaw": _rounded(table["listener_yaws"][frame], 6),
            "reverb": {
                "zone": table["zones"][frame],
                "transition_state": table["transition"][frame],
                "rt60_raw_s": _rounded(table["raw_rt60"][frame]),
                "rt60_s": _rounded(table["rt60"][frame]),
                "wet": _rounded(table["wet"][frame]),
            },
            "sources": [],
        }
        for source in scene["sources"]:
            arrays = table["sources"][source["id"]]
            entry["sources"].append({
                "id": source["id"],
                "occlusion_count": int(arrays["occlusion_count"][frame]),
                "occluded_layers": list(arrays["occluded_layers"][frame]),
                "transmission": _rounded(arrays["transmission"][frame]),
                "detour_m": _rounded(arrays["detour"][frame]),
                "diffraction_db": _rounded(arrays["diffraction_db"][frame]),
                "distance_m": _rounded(arrays["distance"][frame], 6),
                "azimuth_rad": _rounded(arrays["azimuth"][frame]),
                "elevation_rad": _rounded(arrays["elevation"][frame]),
                "itd_s": _rounded(arrays["itd_s"][frame], 12),
                "ild_db": _rounded(arrays["ild_db"][frame]),
                "direct_gain": _rounded(arrays["direct_gain"][frame]),
            })
        frames.append(entry)

    master_hash = sha256_file(result.master_path)
    master_audio, _ = read_wav(result.master_path)
    peak = float(np.max(np.abs(master_audio)))
    report = {
        "schema_version": 1,
        "scene_summary": {
            "digest": result.digest,
            "seed": int(scene["project"]["seed"]),
            "sample_rate": int(scene["project"]["sample_rate"]),
            "frame_rate": float(scene["project"]["frame_rate"]),
            "source_ids": [source["id"] for source in scene["sources"]],
            "layer_ids": [layer["id"] for layer in scene["layers"]],
        },
        "master": {
            "file": "master.wav",
            "sha256": master_hash,
            "block_count": int(result.block_count),
            "total_samples": int(result.total_samples),
            "peak_amplitude": _rounded(peak, 6),
        },
        "recovery": {
            "resume_count": int(result.resume_count),
            "graceful_interrupt": bool(result.graceful_interrupt),
            "resume_mismatch": bool(result.resume_mismatch),
        },
        "frames": frames,
    }
    return report


def write_report(report: dict, path: str | Path) -> None:
    path = Path(path)
    text = json.dumps(report, indent=2, sort_keys=False, ensure_ascii=False)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(text + "\n", encoding="utf-8")
    tmp.replace(path)


def compare_master_files(reference: str | Path, candidate: str | Path) -> dict:
    """Byte-level comparison of two rendered master WAV files."""
    reference_bytes = Path(reference).read_bytes()
    candidate_bytes = Path(candidate).read_bytes()
    if reference_bytes == candidate_bytes:
        return {"identical": True, "sha256_reference": hashlib.sha256(reference_bytes).hexdigest(),
                "sha256_candidate": hashlib.sha256(candidate_bytes).hexdigest(), "differences": []}
    differences = []
    ref_header = np.frombuffer(reference_bytes[:44], dtype=np.uint8)
    cand_header = np.frombuffer(candidate_bytes[:44], dtype=np.uint8)
    if not np.array_equal(ref_header, cand_header):
        differences.append({"block": None, "field": "wav_header",
                            "message": "WAV headers differ"})
    ref = np.frombuffer(reference_bytes[44:], dtype=np.float32)
    cand = np.frombuffer(candidate_bytes[44:], dtype=np.float32)
    if ref.size != cand.size:
        differences.append({"block": None, "field": "total_samples",
                            "reference": int(ref.size // 2), "candidate": int(cand.size // 2)})
    common = min(ref.size, cand.size)
    mismatch = np.flatnonzero(ref[:common] != cand[:common])
    block_samples = None
    if mismatch.size:
        first = int(mismatch[0])
        differences.append({
            "block": first // 2,
            "sample_index": first // 2,
            "channel": int(first % 2),
            "reference": float(ref[first]),
            "candidate": float(cand[first]),
        })
        for index in mismatch[:16]:
            differences.append({
                "block": int(index // 2),
                "sample_index": int(index // 2),
                "channel": int(index % 2),
                "reference": float(ref[index]),
                "candidate": float(cand[index]),
            })
    if not differences:
        differences.append({"block": None, "field": "bytes", "message": "files differ in length only"})
    return {
        "identical": False,
        "sha256_reference": hashlib.sha256(reference_bytes).hexdigest(),
        "sha256_candidate": hashlib.sha256(candidate_bytes).hexdigest(),
        "differences": differences,
        "block_samples_hint": block_samples,
    }


def assert_masters_match(reference: str | Path, candidate: str | Path) -> dict:
    comparison = compare_master_files(reference, candidate)
    if not comparison["identical"]:
        first = comparison["differences"][0]
        raise BusinessError(
            MASTER_MISMATCH,
            f"$.master.block[{first.get('block')}]",
            f"resume master differs: {first.get('field', 'sample')}",
        )
    return comparison

