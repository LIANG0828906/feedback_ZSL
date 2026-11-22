from __future__ import annotations

from pathlib import Path

import numpy as np
import pytest

from edgepipe.diff import first_mask_difference, raise_for_mask_difference
from edgepipe.errors import EdgePipeError, ErrorCode
from edgepipe.ops import BlockGrid
from edgepipe.verify import verify_three_paths


def test_first_difference_reports_pixel_block_and_stage():
    first = np.zeros((64, 64), dtype=np.uint8)
    second = first.copy()
    second[17, 23] = 255
    grid = BlockGrid.create(64, 64, 16)
    difference = first_mask_difference(
        first, second, stage="thin", grid=grid
    )
    assert difference is not None
    assert difference.y == 17
    assert difference.x == 23
    assert difference.block_id == "b0005"
    assert difference.stage == "thin"

    with pytest.raises(EdgePipeError) as caught:
        raise_for_mask_difference(first, second, stage="thin", grid=grid)
    assert caught.value.code == ErrorCode.DIFF
    assert caught.value.details["coordinate"] == {"y": 17, "x": 23}
    assert caught.value.details["block_id"] == "b0005"


def test_three_paths_and_boundary_acceptance(
    tmp_path: Path, first_sample, samples_dir: Path
):
    report = verify_three_paths(
        first_sample,
        samples_dir,
        tmp_path / "verification",
        prefix_commits=7,
        save_masks=False,
    )
    assert report["all_pass"]
    assert report["three_paths_masks_equal"]
    assert report["three_paths_stages_equal"]
    assert report["three_paths_geometry_equal"]
    assert report["three_paths_commit_sequence_equal"]
    assert report["chunk_vs_whole_stages_equal"]
    assert report["chunk_vs_whole_max_pixel_difference"] == 0
    assert report["geometry_all_pass"]
    assert report["paths"]["incremental"]["restored_commits"] == 7
    assert report["paths"]["resume"]["restored_commits"] == 7
    assert report["checkpoint_invalidation"]["all_invalidations_detected"]

