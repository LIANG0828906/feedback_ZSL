import json
import threading

from ocr_pipeline import engine
from ocr_pipeline.pipeline import Pipeline, PipelineConfig


class CountdownCancel:
    """Trips after N is_set() calls to simulate a mid-run pause/kill."""

    def __init__(self, limit):
        self._limit = limit
        self._calls = 0
        self._event = threading.Event()

    def is_set(self):
        self._calls += 1
        if self._calls > self._limit:
            self._event.set()
        return self._event.is_set()


def _config(data_dir, **overrides):
    params = dict(
        data_dir=data_dir,
        source_version="3.0",
        merge_threshold=engine.DEFAULT_MERGE_THRESHOLD,
        conf_threshold=engine.DEFAULT_CONF_THRESHOLD,
        lines_per_interval=1,
        max_workers=1,
        buffer_limit=1,
    )
    params.update(overrides)
    return PipelineConfig(**params)


def _total_intervals(pipeline):
    manifest = json.loads(
        (pipeline.data_dir / "manifest.json").read_text(encoding="utf-8"))
    total = 0
    for entry in manifest["images"]:
        gt = json.loads(
            (pipeline.data_dir / "ground_truth" / f"{entry['image_id']}.json")
            .read_text(encoding="utf-8"))
        total += len(engine.detect_and_recognize(entry["image_id"], [
            engine.GtRegion(
                quad=tuple(tuple(p) for p in r["quad"]),
                text=r["text"],
                char_count=r["char_count"],
                avg_stroke_width=r["avg_stroke_width"],
            )
            for r in gt["regions"]
        ]))
    return total


def test_pause_and_resume_completes(dataset):
    pipeline = Pipeline(_config(dataset))
    total = _total_intervals(pipeline)
    assert total > 3

    summary = pipeline.run(cancel_event=CountdownCancel(3))
    assert summary["completed"] is False
    partial = sum(
        img["committed_intervals"] for img in summary["images"].values())
    assert 0 < partial < total

    # Restart: a brand-new Pipeline instance resumes from persisted state.
    resumed = Pipeline(_config(dataset))
    summary2 = resumed.run()
    assert summary2["completed"] is True
    done = sum(
        img["committed_intervals"] for img in summary2["images"].values())
    assert done == total


def test_param_version_mismatch_invalidates_snapshot(dataset):
    Pipeline(_config(dataset)).run()
    store_dir = dataset / "checkpoints"
    assert any(store_dir.iterdir())

    changed = Pipeline(_config(dataset, conf_threshold=0.9))
    summary = changed.run()
    assert summary["completed"] is True
    # Snapshot was invalidated and fully re-recognized with the new params.
    manifest = json.loads(
        (store_dir / "sample_000" / "manifest.json").read_text(encoding="utf-8"))
    assert manifest["param_version"] == changed.param_version


def test_content_hash_mismatch_invalidates_snapshot(dataset):
    Pipeline(_config(dataset)).run()
    # Tamper with one image: content hash no longer matches the snapshot.
    image_path = dataset / "images" / "sample_000.png"
    image_path.write_bytes(image_path.read_bytes() + b"tamper")
    rerun = Pipeline(_config(dataset))
    summary = rerun.run()
    assert summary["completed"] is True


def test_commit_records_carry_required_versions(dataset):
    Pipeline(_config(dataset)).run()
    record = json.loads(
        (dataset / "checkpoints" / "sample_000" / "intervals" / "0000.json")
        .read_text(encoding="utf-8"))
    for key in ("content_hash", "source_version", "param_version",
                "algo_version", "commit_seq", "lines"):
        assert key in record
    seqs = []
    for image_dir in sorted((dataset / "checkpoints").iterdir()):
        for path in sorted((image_dir / "intervals").glob("*.json")):
            seqs.append(json.loads(path.read_text())["commit_seq"])
    assert seqs == sorted(seqs)  # stable, canonical commit order
