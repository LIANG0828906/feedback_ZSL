from __future__ import annotations

import argparse
import hashlib
import os
import sys
import time
from pathlib import Path

import numpy as np

from train.common import atomic_write_json, load_config, resolve_path


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        while True:
            block = handle.read(1024 * 1024)
            if not block:
                break
            digest.update(block)
    return digest.hexdigest()


def generate(config_path: str, force: bool = False) -> dict:
    config = load_config(config_path)
    manifest = __import__("json").load(
        open(resolve_path(config["model_manifest"]), encoding="utf-8")
    )
    dataset = manifest["dataset"]
    data_dir = resolve_path(config["data"]["directory"])
    metadata_path = resolve_path(config["data"]["metadata"])
    data_dir.mkdir(parents=True, exist_ok=True)

    train_samples = int(dataset["train_samples"])
    validation_samples = int(dataset["validation_samples"])
    feature_dim = int(dataset["feature_dim"])
    classes = int(dataset["num_classes"])
    seed = int(dataset["generator_seed"])
    shard_count = len(config["workers"])
    shard_sizes = [
        train_samples // shard_count + (1 if index < train_samples % shard_count else 0)
        for index in range(shard_count)
    ]

    expected_files = [
        data_dir / f"train_features_{index}.npy" for index in range(shard_count)
    ] + [data_dir / "validation_features.npy"]
    if not force and metadata_path.exists() and all(path.exists() for path in expected_files):
        with open(metadata_path, "r", encoding="utf-8") as handle:
            metadata = __import__("json").load(handle)
        if (
            metadata.get("generator_seed") == seed
            and metadata.get("train_samples") == train_samples
            and len(metadata.get("shards", [])) == shard_count
            and all(
                shard["sample_count"] == shard_sizes[index]
                for index, shard in enumerate(metadata.get("shards", []))
            )
        ):
            print(f"data already present: {metadata_path}", flush=True)
            return metadata

    started = time.perf_counter()
    rng = np.random.default_rng(seed)
    centroids = rng.normal(
        0.0, float(dataset["centroid_scale"]), size=(classes, feature_dim)
    ).astype(np.float32)
    labels = rng.integers(0, classes, size=train_samples, dtype=np.int16)
    order = rng.permutation(train_samples)
    labels = labels[order]
    noise_scale = float(dataset["noise_scale"])

    staging = data_dir / "train_features_staging.npy"
    staging_labels = data_dir / "train_labels_staging.npy"
    if staging.exists():
        staging.unlink()
    if staging_labels.exists():
        staging_labels.unlink()
    features = np.lib.format.open_memmap(
        staging, mode="w+", dtype=np.float32, shape=(train_samples, feature_dim)
    )
    chunk_size = 8192
    for start in range(0, train_samples, chunk_size):
        stop = min(start + chunk_size, train_samples)
        chunk_labels = labels[start:stop]
        features[start:stop] = centroids[chunk_labels]
        features[start:stop] += rng.normal(
            0.0, noise_scale, size=(stop - start, feature_dim)
        ).astype(np.float32)
    features.flush()
    del features
    np.save(staging_labels, labels, allow_pickle=False)

    shard_records = []
    shard_global = np.load(staging, mmap_mode="r")
    shard_labels = np.load(staging_labels, mmap_mode="r")
    offset = 0
    for shard_index, sample_count in enumerate(shard_sizes):
        feature_path = data_dir / f"train_features_{shard_index}.npy"
        label_path = data_dir / f"train_labels_{shard_index}.npy"
        shard_features = np.lib.format.open_memmap(
            feature_path,
            mode="w+",
            dtype=np.float32,
            shape=(sample_count, feature_dim),
        )
        shard_features[:] = shard_global[offset : offset + sample_count]
        shard_features.flush()
        del shard_features
        np.save(label_path, shard_labels[offset : offset + sample_count], allow_pickle=False)
        shard_records.append(
            {
                "shard_index": shard_index,
                "sample_count": sample_count,
                "features_file": feature_path.name,
                "labels_file": label_path.name,
                "features_sha256": _sha256_file(feature_path),
                "labels_sha256": _sha256_file(label_path),
            }
        )
        offset += sample_count
    del shard_global, shard_labels
    staging.unlink()
    staging_labels.unlink()

    validation_labels = rng.integers(
        0, classes, size=validation_samples, dtype=np.int16
    )
    validation_features = np.empty(
        (validation_samples, feature_dim), dtype=np.float32
    )
    for start in range(0, validation_samples, chunk_size):
        stop = min(start + chunk_size, validation_samples)
        chunk_labels = validation_labels[start:stop]
        validation_features[start:stop] = centroids[chunk_labels]
        validation_features[start:stop] += rng.normal(
            0.0, noise_scale, size=(stop - start, feature_dim)
        ).astype(np.float32)
    validation_path = data_dir / "validation_features.npy"
    validation_labels_path = data_dir / "validation_labels.npy"
    np.save(validation_path, validation_features, allow_pickle=False)
    np.save(validation_labels_path, validation_labels, allow_pickle=False)

    metadata = {
        "format_version": 1,
        "generator_seed": seed,
        "train_samples": train_samples,
        "validation_samples": validation_samples,
        "feature_dim": feature_dim,
        "num_classes": classes,
        "shards": shard_records,
        "validation_features_file": validation_path.name,
        "validation_labels_file": validation_labels_path.name,
        "validation_features_sha256": _sha256_file(validation_path),
        "validation_labels_sha256": _sha256_file(validation_labels_path),
        "generation_seconds": round(time.perf_counter() - started, 3),
    }
    atomic_write_json(metadata_path, metadata)
    print(
        f"generated {train_samples} samples in {metadata['generation_seconds']}s; "
        f"shards={shard_sizes}",
        flush=True,
    )
    return metadata


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate deterministic synthetic data")
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--force", action="store_true")
    args = parser.parse_args()
    generate(args.config, force=args.force)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
