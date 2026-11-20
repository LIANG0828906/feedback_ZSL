from __future__ import annotations

from pathlib import Path

import pytest

from mixrec.engine import MixEngine
from mixrec.errors import E_SLICE_GAP, MixError
from mixrec.index import load_index, rebuild_chain, validate_index
from tests.conftest import job_config_path


def test_index_gap_is_located(fixtures: Path, tmp_path: Path):
    config_path = job_config_path(fixtures, "short_timeline")
    output = tmp_path / "output"
    engine = MixEngine.from_file(
        config_path, output_dir_override=output, trace_override=tmp_path / "trace.jsonl"
    )
    assert engine.run(resume=False).ok
    index = load_index(output / "index.json")
    index["slices"][2]["start_sample"] += 1
    rebuild_chain(index)
    with pytest.raises(MixError) as caught:
        validate_index(index, output_dir=output, verify_files=False)
    assert caught.value.code == E_SLICE_GAP
    assert caught.value.slice_index == 2
