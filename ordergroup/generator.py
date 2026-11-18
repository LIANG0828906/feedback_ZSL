from __future__ import annotations

import hashlib
import json
import random
import struct
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any, BinaryIO


@dataclass
class GenerationOptions:
    count: int
    seed: int
    topic: str
    partitions: int
    backend: str
    bootstrap_servers: str
    output_dir: Path
    batch_size: int = 500
    jsonl_path: Path | None = None
    dry_run: bool = False


def _event_at(index: int) -> dict[str, Any]:
    timestamp = datetime(2025, 1, 1, tzinfo=UTC) + timedelta(milliseconds=index * 10)
    return {
        "order_id": f"ORD-{index + 1:010d}",
        "amount": round(1.0 + (index * 7919 % 1000000) / 100.0, 2),
        "event_time": timestamp.isoformat(timespec="milliseconds").replace(
            "+00:00", "Z"
        ),
    }


def _event(index: int, rng: random.Random) -> dict[str, Any]:
    # The order and event-time fields are derived from the index so the stream
    # remains reproducible even if random-number implementations change.
    timestamp = datetime(2025, 1, 1, tzinfo=UTC) + timedelta(milliseconds=index * 10)
    amount = round(rng.uniform(1.0, 9999.99), 2)
    return {
        "order_id": f"ORD-{index + 1:010d}",
        "amount": amount,
        "event_time": timestamp.isoformat(timespec="milliseconds").replace(
            "+00:00", "Z"
        ),
    }


def _canonical(event: dict[str, Any]) -> bytes:
    return json.dumps(
        event, ensure_ascii=False, sort_keys=True, separators=(",", ":")
    ).encode("utf-8")


def generate_events(options: GenerationOptions, backend: Any = None) -> dict[str, Any]:
    if options.count < 0:
        raise ValueError("count must be non-negative")
    if options.partitions <= 0:
        raise ValueError("partitions must be positive")
    if options.batch_size <= 0:
        raise ValueError("batch_size must be positive")
    if options.count > 10_000_000:
        raise ValueError("count exceeds the local safety limit of 10,000,000")
    rng = random.Random(options.seed)
    digest = hashlib.sha256()
    partition_counts = [0] * options.partitions
    partition_hashes = [hashlib.sha256() for _ in range(options.partitions)]
    first_event: dict[str, Any] | None = None
    last_event: dict[str, Any] | None = None

    data_handles: list[BinaryIO] = []
    index_handles: list[BinaryIO] = []
    kafka_batches: list[list[tuple[str | None, bytes]]] = [
        [] for _ in range(options.partitions)
    ]
    jsonl_handle: BinaryIO | None = None

    simulation = options.backend == "simulator" and not options.dry_run
    kafka = options.backend == "kafka" and not options.dry_run
    if simulation:
        output = options.output_dir
        output.mkdir(parents=True, exist_ok=True)
        for candidate in output.iterdir():
            if candidate.name.startswith("partition-") and candidate.suffix in (
                ".jsonl",
                ".idx",
            ):
                candidate.unlink()
        manifest_path = output / "manifest.json"
        if manifest_path.exists():
            manifest_path.unlink()
        for partition in range(options.partitions):
            data_handles.append(
                (output / f"partition-{partition:02d}.jsonl").open("wb")
            )
            index_handles.append(
                (output / f"partition-{partition:02d}.idx").open("wb")
            )
    if options.jsonl_path is not None and not options.dry_run:
        options.jsonl_path.parent.mkdir(parents=True, exist_ok=True)
        jsonl_handle = options.jsonl_path.open("wb")

    try:
        for index in range(options.count):
            event = _event(index, rng)
            if first_event is None:
                first_event = event
            last_event = event
            partition = index % options.partitions
            encoded = _canonical(event)
            digest.update(encoded + b"\n")
            partition_hashes[partition].update(encoded + b"\n")
            partition_counts[partition] += 1
            if simulation:
                handle = data_handles[partition]
                position = handle.tell()
                handle.write(encoded)
                handle.write(b"\n")
                index_handles[partition].write(struct.pack(">q", position))
            elif kafka:
                batch = kafka_batches[partition]
                batch.append((event["order_id"], encoded))
                if len(batch) >= options.batch_size:
                    if backend is None:
                        raise RuntimeError("Kafka backend is required")
                    backend.produce(partition, batch)
                    kafka_batches[partition] = []
            if jsonl_handle is not None:
                jsonl_handle.write(encoded)
                jsonl_handle.write(b"\n")
        if kafka and backend is not None:
            for partition, batch in enumerate(kafka_batches):
                if batch:
                    backend.produce(partition, batch)
    finally:
        for handle in data_handles + index_handles:
            handle.close()
        if jsonl_handle is not None:
            jsonl_handle.close()

    manifest: dict[str, Any] = {
        "format": "order-events-v1",
        "fields": {
            "order_id": "string",
            "amount": "number",
            "event_time": "ISO-8601 UTC string",
        },
        "topic": options.topic,
        "backend": options.backend,
        "seed": options.seed,
        "count": options.count,
        "partitions": options.partitions,
        "batch_size": options.batch_size,
        "partition_counts": partition_counts,
        "partition_sha256": [
            item.hexdigest() for item in partition_hashes
        ],
        "stream_sha256": digest.hexdigest(),
        "first_event": first_event,
        "last_event": last_event,
        "dry_run": options.dry_run,
    }
    if simulation:
        manifest_path = options.output_dir / "manifest.json"
        manifest_path.write_text(
            json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        manifest["manifest_path"] = str(manifest_path)
    if options.jsonl_path is not None and not options.dry_run:
        manifest["jsonl_path"] = str(options.jsonl_path)
    return manifest

