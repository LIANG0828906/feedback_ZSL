from __future__ import annotations

import json
import shutil
from pathlib import Path

import cv2
import numpy as np

from panoworkbench.config import FeatureConfig, PipelineConfig
from panoworkbench.dataset import load_dataset
from panoworkbench.pipeline import StitchPipeline
from panoworkbench.report import canonical_report, compare_panorama_outputs


def test_realtime_resume_and_full_recompute_are_identical(
    sample_dataset, tmp_path: Path
) -> None:
    config = PipelineConfig()
    realtime = StitchPipeline(
        sample_dataset, config, tmp_path / "realtime", resume=False
    ).run()
    partial = StitchPipeline(
        sample_dataset, config, tmp_path / "resume", resume=True
    ).run(pause_after_pairs=1)
    assert partial.status == "paused"
    assert partial.report["progress"]["panorama_complete"] is False
    assert not (tmp_path / "resume" / "panorama.png").exists()
    resumed = StitchPipeline(
        sample_dataset, config, tmp_path / "resume", resume=True
    ).run()
    full = StitchPipeline(
        sample_dataset, config, tmp_path / "full", resume=False
    ).run()
    assert realtime.status == resumed.status == full.status == "completed"
    assert resumed.resumed_pairs == 1
    for other in ("resume", "full"):
        comparison = compare_panorama_outputs(
            tmp_path / "realtime", tmp_path / other
        )
        assert comparison.identical is True
        assert comparison.report_identical is True
        assert comparison.first_diff_xy is None
    report_a = json.loads((tmp_path / "realtime" / "report.json").read_text())
    report_b = json.loads((tmp_path / "resume" / "report.json").read_text())
    assert canonical_report(report_a) == canonical_report(report_b)


def test_first_difference_reports_pixel_and_pair(
    sample_dataset, tmp_path: Path
) -> None:
    result = StitchPipeline(
        sample_dataset, PipelineConfig(), tmp_path / "base", resume=False
    ).run()
    assert result.panorama is not None
    changed = tmp_path / "changed"
    shutil.copytree(tmp_path / "base", changed)
    image = cv2.imread(str(changed / "panorama.png"), cv2.IMREAD_COLOR)
    with np.load(tmp_path / "base" / "owner.npz") as data:
        owner = data["owner"]
    candidates = np.argwhere(owner >= 0)
    assert len(candidates) > 0
    y, x = (int(value) for value in candidates[len(candidates) // 2])
    image[y, x] = (0, 0, 0) if not np.array_equal(image[y, x], (0, 0, 0)) else (1, 1, 1)
    cv2.imwrite(str(changed / "panorama.png"), image)
    comparison = compare_panorama_outputs(tmp_path / "base", changed)
    assert comparison.identical is False
    assert comparison.first_diff_xy == (x, y)
    assert comparison.owner_image is not None
    assert comparison.pair_id is not None


def test_parameter_version_mismatch_invalidates_whole_snapshot(
    sample_dataset, tmp_path: Path
) -> None:
    output = tmp_path / "checkpointed"
    first = StitchPipeline(
        sample_dataset, PipelineConfig(), output, resume=False
    ).run()
    assert first.status == "completed"
    changed_config = PipelineConfig(
        features=FeatureConfig(
            max_features=1200,
            octave_layers=4,
            contrast_threshold=0.012,
            ratio_test=0.76,
            min_matches=12,
        )
    )
    resumed = StitchPipeline(
        sample_dataset, changed_config, output, resume=True
    ).run()
    assert resumed.resumed_pairs == 0
    assert resumed.checkpoint_invalid_reason is not None
    assert "parameter version mismatch" in resumed.checkpoint_invalid_reason
    assert resumed.status == "completed"


def test_content_hash_mismatch_invalidates_whole_snapshot(
    sample_root: Path, tmp_path: Path
) -> None:
    root = tmp_path / "dataset"
    shutil.copytree(sample_root, root)
    dataset = load_dataset(root)
    output = tmp_path / "content"
    first = StitchPipeline(dataset, PipelineConfig(), output, resume=False).run()
    assert first.status == "completed"

    image_path = root / "images" / "view_001.png"
    image = cv2.imread(str(image_path), cv2.IMREAD_COLOR)
    image[0, 0] = (image[0, 0].astype(np.int16) + 17).astype(np.uint8)
    cv2.imwrite(str(image_path), image)
    changed_dataset = load_dataset(root)
    assert changed_dataset.content_hash != dataset.content_hash
    resumed = StitchPipeline(
        changed_dataset, PipelineConfig(), output, resume=True
    ).run()
    assert resumed.resumed_pairs == 0
    assert resumed.checkpoint_invalid_reason is not None
    assert "dataset content hash mismatch" in resumed.checkpoint_invalid_reason


def test_new_run_removes_stale_completion_before_pausing(
    sample_dataset, tmp_path: Path
) -> None:
    output = tmp_path / "stale"
    completed = StitchPipeline(
        sample_dataset, PipelineConfig(), output, resume=False
    ).run()
    assert completed.status == "completed"
    assert (output / "panorama.png").is_file()
    paused = StitchPipeline(
        sample_dataset, PipelineConfig(), output, resume=False
    ).run(pause_after_pairs=1)
    assert paused.status == "paused"
    assert paused.report["status"] == "paused"
    assert paused.report["progress"]["panorama_complete"] is False
    assert not (output / "panorama.png").exists()
    run_state = json.loads((output / "run_state.json").read_text())
    assert run_state["status"] == "paused"
