"""Offline deterministic generator for ONNX models, protocol maps and samples.

Everything is derived from a single integer seed so repeated runs produce
byte-identical artifacts (ONNX graphs carry fixed weights, no timestamps).
"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np

SEED = 20261119

V3_COLUMNS = (
    "c_user_age",
    "c_user_tenure",
    "c_item_age",
    "c_item_price",
    "c_ctr_7d",
    "c_cvr_7d",
    "c_match_score",
    "c_dwell_mean",
)
V4_COLUMNS = (
    "c_item_price",
    "c_item_age",
    "c_match_score",
    "c_ctr_7d",
    "c_cvr_7d",
    "c_dwell_mean",
    "c_user_age",
    "c_user_tenure",
    "c_emb_cosine",
    "c_ctx_hour",
)

VERSIONS = {
    "v3": {
        "columns": V3_COLUMNS,
        "protocol_version": "rank-features-v1",
        "model_file": "ranker_v3.onnx",
    },
    "v4": {
        "columns": V4_COLUMNS,
        "protocol_version": "rank-features-v2",
        "model_file": "ranker_v4.onnx",
    },
}

TENANTS = ("tenant-a", "tenant-b")
CLIENTS = tuple(f"client-{index:02d}" for index in range(1, 6))
SAMPLE_COUNT = 40
MISORDERED_IDS = (7, 17, 27, 37)


def build_onnx_model(path: Path, feature_count: int, rng: np.random.Generator) -> None:
    """Build features[M,K] -> sigmoid(features @ W + b) with fixed weights."""

    import onnx
    from onnx import TensorProto, helper, numpy_helper

    weights = rng.standard_normal((feature_count, 1)).astype(np.float32) * 0.25
    bias = rng.standard_normal(1).astype(np.float32) * 0.1

    x = helper.make_tensor_value_info("features", TensorProto.FLOAT, [None, feature_count])
    y = helper.make_tensor_value_info("score", TensorProto.FLOAT, [None, 1])

    w_init = numpy_helper.from_array(weights, name="W")
    b_init = numpy_helper.from_array(bias, name="b")

    matmul = helper.make_node("MatMul", ["features", "W"], ["logits"], name="matmul")
    add = helper.make_node("Add", ["logits", "b"], ["biased"], name="bias_add")
    sigmoid = helper.make_node("Sigmoid", ["biased"], ["score"], name="sigmoid")

    graph = helper.make_graph(
        [matmul, add, sigmoid],
        "ranker_graph",
        [x],
        [y],
        initializer=[w_init, b_init],
    )
    opset = helper.make_opsetid("", 13)
    model = helper.make_model(graph, opset_imports=[opset], producer_name="deterministic-generator")
    model.ir_version = 8
    onnx.checker.check_model(model)
    path.write_bytes(model.SerializeToString())


def _misordered_columns(columns: tuple[str, ...], rng: np.random.Generator) -> list[str]:
    """Swap two distinct positions so the declared order becomes wrong."""

    shuffled = list(columns)
    left, right = rng.choice(len(columns), size=2, replace=False)
    shuffled[left], shuffled[right] = shuffled[right], shuffled[left]
    return shuffled


def generate(output_dir: str | Path) -> dict:
    rng = np.random.default_rng(SEED)
    output_dir = Path(output_dir)
    model_dir = output_dir / "models"
    sample_dir = output_dir / "samples"
    model_dir.mkdir(parents=True, exist_ok=True)
    sample_dir.mkdir(parents=True, exist_ok=True)

    mapping = {}
    for version, spec in VERSIONS.items():
        build_onnx_model(model_dir / spec["model_file"], len(spec["columns"]), rng)
        mapping[version] = {
            "columns": list(spec["columns"]),
            "model_file": spec["model_file"],
            "protocol_version": spec["protocol_version"],
        }
    with open(model_dir / "protocol_mapping.json", "w", encoding="utf-8") as handle:
        json.dump(mapping, handle, indent=2, sort_keys=True)
        handle.write("\n")

    requests = []
    misordered_ids = []
    for index in range(SAMPLE_COUNT):
        version = "v3" if rng.random() < 0.5 else "v4"
        spec = VERSIONS[version]
        tenant = TENANTS[index % len(TENANTS)]
        client = CLIENTS[index % len(CLIENTS)]
        columns = list(spec["columns"])
        misordered = index in MISORDERED_IDS
        if misordered:
            columns = _misordered_columns(spec["columns"], rng)
            misordered_ids.append(f"req-{index:04d}")
        features = [round(float(value), 6) for value in rng.standard_normal(len(columns))]
        requests.append(
            {
                "client_id": client,
                "columns": columns,
                "features": features,
                "misordered": misordered,
                "protocol_version": spec["protocol_version"],
                "request_id": f"req-{index:04d}",
                "tenant_id": tenant,
            }
        )

    sample_file = sample_dir / "sample_requests.jsonl"
    with open(sample_file, "w", encoding="utf-8") as handle:
        for item in requests:
            handle.write(json.dumps(item, sort_keys=True) + "\n")

    manifest = {
        "count": SAMPLE_COUNT,
        "misordered_request_ids": misordered_ids,
        "protocols": {
            version: spec["protocol_version"] for version, spec in VERSIONS.items()
        },
        "sample_file": "sample_requests.jsonl",
        "seed": SEED,
    }
    with open(sample_dir / "manifest.json", "w", encoding="utf-8") as handle:
        json.dump(manifest, handle, indent=2, sort_keys=True)
        handle.write("\n")
    return manifest


if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(description="Generate deterministic gateway artifacts")
    parser.add_argument("--output", default="generated")
    args = parser.parse_args()
    result = generate(args.output)
    print(json.dumps(result, indent=2, sort_keys=True))
