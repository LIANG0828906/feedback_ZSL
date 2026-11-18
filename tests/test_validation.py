from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import soundfile as sf

from conftest import run_cli, write_manifest_variant


def _errors(result) -> list[dict]:
    return json.loads(result.stderr)["errors"]


def _has_code(result, code: str, mic: str | None = None) -> bool:
    return any(
        item["code"] == code and (mic is None or item["mic"] == mic)
        for item in _errors(result)
    )


def test_missing_file_is_structured(workspace: Path, manifest_path: Path, tmp_path: Path) -> None:
    variant = write_manifest_variant(
        manifest_path,
        tmp_path / "missing.json",
        lambda document: document["microphones"][0].__setitem__(
            "path", "audio/not-present.wav"
        ),
    )
    result = run_cli(["validate", "--manifest", str(variant)])
    mic = json.load(open(variant, encoding="utf-8"))["microphones"][0]["name"]
    assert result.returncode != 0
    assert _has_code(result, "E_FILE_MISSING", mic)


def test_rate_depth_and_channel_mismatches(workspace: Path, manifest_path: Path, tmp_path: Path) -> None:
    source, _ = sf.read(workspace / "audio/sop_a.wav")
    sf.write(workspace / "audio" / "bad-rate.wav", source, 44100, subtype="PCM_24")
    sf.write(workspace / "audio" / "bad-depth.wav", source, 48000, subtype="PCM_16")
    sf.write(
        workspace / "audio" / "bad-channels.wav",
        np.column_stack((source, source)),
        48000,
        subtype="PCM_24",
    )
    cases = [
        ("bad-rate.wav", "E_SAMPLE_RATE_MISMATCH"),
        ("bad-depth.wav", "E_BIT_DEPTH_MISMATCH"),
        ("bad-channels.wav", "E_CHANNELS_MISMATCH"),
    ]
    for filename, code in cases:
        def mutate(document: dict, name: str = filename) -> None:
            document["microphones"][0]["path"] = f"audio/{name}"

        variant = write_manifest_variant(
            manifest_path,
            workspace / f"{code}.json",
            mutate,
        )
        result = run_cli(["validate", "--manifest", str(variant)])
        assert result.returncode != 0
        assert _has_code(result, code)


def test_invalid_voice_ratio_sum_and_band(workspace: Path, manifest_path: Path, tmp_path: Path) -> None:
    voice_variant = write_manifest_variant(
        manifest_path,
        workspace / "voice.json",
        lambda document: document["microphones"][0].__setitem__("voice", "无效声部"),
    )
    mic = json.load(open(voice_variant, encoding="utf-8"))["microphones"][0]["name"]
    result = run_cli(["validate", "--manifest", str(voice_variant)])
    assert _has_code(result, "E_VOICE_INVALID", mic)

    ratio_variant = write_manifest_variant(
        manifest_path,
        workspace / "ratio.json",
        lambda document: document["voices"][0].__setitem__(
            "target_energy_ratio", 0.30
        ),
    )
    result = run_cli(["validate", "--manifest", str(ratio_variant)])
    assert _has_code(result, "E_RATIO_SUM")

    missing_voice_variant = write_manifest_variant(
        manifest_path,
        workspace / "missing-voice.json",
        lambda document: document["voices"].pop(1),
    )
    result = run_cli(["validate", "--manifest", str(missing_voice_variant)])
    assert _has_code(result, "E_VOICE_MISSING")

    band_variant = write_manifest_variant(
        manifest_path,
        workspace / "band.json",
        lambda document: document["voices"][2].__setitem__(
            "breakpoint_band_hz", [1600.0, 400.0]
        ),
    )
    result = run_cli(["validate", "--manifest", str(band_variant)])
    assert _has_code(result, "E_BAND_INVERTED")

