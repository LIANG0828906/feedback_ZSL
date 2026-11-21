from __future__ import annotations

from pathlib import Path

from npr_pipeline.pipeline import RenderPipeline
from npr_pipeline.samples import generate_samples
from npr_pipeline.storage import Store
from npr_pipeline.trace import result_trace_records


def _tree_bytes(root: Path) -> dict[str, bytes]:
    return {
        str(path.relative_to(root)): path.read_bytes()
        for path in sorted(root.rglob("*"))
        if path.is_file()
    }


def test_sample_generation_is_byte_deterministic(tmp_path) -> None:
    first = tmp_path / "first"
    second = tmp_path / "second"
    generate_samples(first, seed=20261122)
    generate_samples(second, seed=20261122)
    assert _tree_bytes(first) == _tree_bytes(second)


def test_trace_contains_required_fields(
    balanced_scene,
    balanced_params,
    tmp_path,
) -> None:
    store = Store(tmp_path / "work")
    result = RenderPipeline(store).render(balanced_scene, balanced_params)
    records = result_trace_records(result, operation="test")
    batch_records = [record for record in records if record["type"] == "batch"]
    assert batch_records
    required = {
        "version",
        "layer",
        "batch",
        "chunk",
        "affected_batches",
        "reused_batches",
        "chunk_count",
        "excluded_strokes",
        "content_hash",
    }
    for record in batch_records:
        assert required <= set(record)
        assert record["version"] == result.version
        assert record["content_hash"]
    version_record = [record for record in records if record["type"] == "version"][-1]
    assert version_record["content_hash"] == result.version_hash

