import json

from ocr_pipeline.pipeline import Pipeline, PipelineConfig


def _run(data_dir, workers, buffer_limit):
    config = PipelineConfig(
        data_dir=data_dir,
        lines_per_interval=1,
        max_workers=workers,
        buffer_limit=buffer_limit,
    )
    pipeline = Pipeline(config)
    summary = pipeline.run()
    assert summary["completed"] is True
    return pipeline


def _records(data_dir):
    out = {}
    root = data_dir / "checkpoints"
    for image_dir in sorted(root.iterdir()):
        for path in sorted((image_dir / "intervals").glob("*.json")):
            out[(image_dir.name, path.stem)] = path.read_bytes()
    return out


def test_parallel_matches_serial_byte_for_byte(dataset, fresh_copy):
    serial = _run(dataset, workers=1, buffer_limit=1)
    parallel = _run(fresh_copy, workers=4, buffer_limit=2)

    serial_records = _records(dataset)
    parallel_records = _records(fresh_copy)
    assert serial_records.keys() == parallel_records.keys()
    for key in serial_records:
        assert serial_records[key] == parallel_records[key], key

    # Same commit order: canonical (image, interval) sequence.
    serial_order = [key for key, _ in sorted(
        ((k, json.loads(v)["commit_seq"]) for k, v in serial_records.items()),
        key=lambda item: item[1])]
    parallel_order = [key for key, _ in sorted(
        ((k, json.loads(v)["commit_seq"]) for k, v in parallel_records.items()),
        key=lambda item: item[1])]
    assert serial_order == parallel_order
    assert serial.store.commit_log == parallel.store.commit_log
