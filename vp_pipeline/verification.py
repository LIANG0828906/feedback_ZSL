"""Local deterministic acceptance checks used by CLI and test reports."""

from __future__ import annotations

import json
import tempfile
from pathlib import Path
from typing import Any

from .checkpoint import CheckpointStore
from .diff import assert_frames_equal
from .export import export_bundle, load_export_bundle
from .loader import load_scene_result
from .instructions import load_frame_instructions, validate_instructions
from .renderer import render_all_frames
from .session import CompositorSession


def _truth_sample_checks(
    samples_directory: Path,
    frames_by_version: dict[int, dict[int, Any]],
) -> int:
    expected = json.loads(
        (samples_directory / "truth" / "expected_frames.json").read_text(
            encoding="utf-8"
        )
    )
    checks = 0
    for version in (1, 2, 3):
        frames = frames_by_version[version]
        for expected_frame in expected["frames"]:
            frame = frames[expected_frame["frame"]]
            if list(frame.visible_layer_ids) != expected_frame["visible_layers"]:
                raise AssertionError(
                    f"visibility truth mismatch version={version} "
                    f"frame={expected_frame['frame']}"
                )
            for sample in expected_frame["samples"]:
                actual = frame.pixels[sample["y"], sample["x"]].tolist()
                if actual != sample["rgba8"]:
                    raise AssertionError(
                        f"sample pixel mismatch version={version} "
                        f"frame={expected_frame['frame']} "
                        f"pixel=({sample['x']},{sample['y']}) "
                        f"actual={actual} expected={sample['rgba8']}"
                    )
                checks += 1
    return checks


def _sequence(
    model: Any,
    directory: str,
    parallel: bool,
) -> tuple[list[int], list[int], list[int], tuple[tuple[int, str], ...]]:
    session = CompositorSession(
        model,
        CheckpointStore(directory),
        max_workers=4,
        max_frame_buffers_per_thread=1,
    )
    first = session.run(parallel=parallel, max_workers=4)
    session.apply_edit("prop_crate", "opacity", 0.41)
    second = session.run(parallel=parallel, max_workers=4)
    hashes = tuple(
        (index, frame.content_hash)
        for index, frame in sorted(session.current_frames().items())
    )
    return (
        list(first.commit_order),
        list(second.commit_order),
        list(second.reused_windows),
        hashes,
    )


def run_verification(samples_directory: str | Path) -> dict[str, Any]:
    samples = Path(samples_directory)
    result: dict[str, Any] = {
        "samples_directory": str(samples.resolve()),
        "versions": {},
    }
    models = {}
    frames_by_version = {}
    mapping_counts = {}
    for version in (1, 2, 3):
        loaded = load_scene_result(samples / f"scene_vpfc{version}.json")
        models[version] = loaded.model
        frames_by_version[version] = render_all_frames(loaded.model)
        mapping_counts[f"vpfc/{version}"] = len(loaded.mapping_trace)
    result["versions"] = {
        "loaded": ["vpfc/1", "vpfc/2", "vpfc/3"],
        "mapping_entries": mapping_counts,
        "layer_order": [layer.layer_id for layer in models[3].layers],
    }
    result["truth_sample_checks"] = _truth_sample_checks(
        samples, frames_by_version
    )
    instructions = load_frame_instructions(samples / "frame_instructions.json")
    validate_instructions(instructions, models[1])
    validate_instructions(instructions, models[2])
    validate_instructions(instructions, models[3])
    result["frame_instruction_count"] = len(instructions.frames)
    canonical_hashes = [
        tuple(
            frame.content_hash
            for _, frame in sorted(frames_by_version[version].items())
        )
        for version in (1, 2, 3)
    ]
    result["cross_version_pixel_equal"] = (
        canonical_hashes[0] == canonical_hashes[1] == canonical_hashes[2]
    )
    if not result["cross_version_pixel_equal"]:
        raise AssertionError("version outputs are not pixel-equal")

    with tempfile.TemporaryDirectory() as incremental_directory:
        session = CompositorSession(
            models[3],
            CheckpointStore(incremental_directory),
            max_workers=3,
        )
        session.run(parallel=False)
        restarted = CompositorSession(
            models[3],
            CheckpointStore(incremental_directory),
            max_workers=3,
        )
        edit = restarted.apply_edit("prop_crate", "opacity", 0.41)
        invalidated = [
            item.window_index
            for item in edit.invalidations
            if item.stage == "composite"
        ]
        incremental = restarted.run(parallel=True, max_workers=3)
        full = render_all_frames(restarted.model)
        assert_frames_equal(incremental.frames, full, restarted.model)
        result["incremental"] = {
            "invalidated_windows": invalidated,
            "recomputed_windows": list(incremental.recomputed_windows),
            "reused_windows": list(incremental.reused_windows),
            "commit_order": list(incremental.commit_order),
            "pixel_equal_to_full": True,
        }

    with tempfile.TemporaryDirectory() as serial_directory, tempfile.TemporaryDirectory() as parallel_directory:
        serial = _sequence(models[3], serial_directory, False)
        parallel = _sequence(models[3], parallel_directory, True)
    result["parallel_matches_serial"] = serial == parallel
    result["parallel_sequence"] = {
        "initial_commits": parallel[0],
        "incremental_commits": parallel[1],
        "reused_windows": parallel[2],
    }
    if serial != parallel:
        raise AssertionError("parallel and serial sequences diverged")

    export_results = []
    with tempfile.TemporaryDirectory() as export_root:
        root = Path(export_root)
        for source_version in (1, 2, 3):
            for target_version in (1, 2, 3):
                output = root / f"v{source_version}_to_v{target_version}"
                exported = export_bundle(
                    models[source_version],
                    frames_by_version[source_version],
                    f"vpfc/{target_version}",
                    output,
                )
                readback, readback_frames, _ = load_export_bundle(output)
                assert_frames_equal(
                    readback_frames,
                    frames_by_version[source_version],
                    models[source_version],
                )
                if (
                    readback.timeline.fps_num,
                    readback.timeline.fps_den,
                ) != (
                    models[source_version].timeline.fps_num,
                    models[source_version].timeline.fps_den,
                ):
                    raise AssertionError("export changed frame rate")
                export_results.append(
                    {
                        "source": f"vpfc/{source_version}",
                        "target": f"vpfc/{target_version}",
                        "warnings": len(exported.warnings),
                        "pixel_equal": True,
                    }
                )
    result["exports"] = export_results
    result["export_combinations"] = len(export_results)
    result["lossy_warning_total"] = sum(
        item["warnings"] for item in export_results
    )

    with tempfile.TemporaryDirectory() as checkpoint_directory:
        first = CompositorSession(
            models[2], CheckpointStore(checkpoint_directory)
        )
        first.run(parallel=False)
        restored = CompositorSession(
            models[2], CheckpointStore(checkpoint_directory)
        )
        frame_path = Path(checkpoint_directory) / "window_0000.bin"
        with frame_path.open("r+b") as handle:
            handle.seek(3)
            byte = handle.read(1)
            handle.seek(3)
            handle.write(bytes([byte[0] ^ 0xFF]))
        discarded = CompositorSession(
            models[2], CheckpointStore(checkpoint_directory)
        )
        result["checkpoint"] = {
            "restored_windows": sorted(restored.recovery.restored),
            "tampered_snapshot_discarded": discarded.recovery.discarded,
            "discarded_progress": list(discarded.progress),
        }
        if sorted(restored.recovery.restored) != [0, 1, 2, 3, 4, 5]:
            raise AssertionError("matching checkpoint windows were not restored")
        if not discarded.recovery.discarded:
            raise AssertionError("tampered checkpoint was not discarded")
    return result
