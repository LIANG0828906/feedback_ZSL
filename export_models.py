from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path

import numpy as np
import onnx
from onnx import TensorProto, helper, numpy_helper

PROTOCOLS = {
    "v3": ("ranking-features-v1", 3103, 8),
    "v4": ("ranking-features-v2", 4104, 10),
}


def build_model(
    version: str, protocol_version: str, seed: int, feature_count: int
) -> onnx.ModelProto:
    rng = np.random.default_rng(seed)
    weights = rng.normal(0.0, 0.35, size=(feature_count, 1)).astype(np.float32)
    bias = rng.normal(0.0, 0.1, size=(1,)).astype(np.float32)

    input_info = helper.make_tensor_value_info(
        "features", TensorProto.FLOAT, ["batch", feature_count]
    )
    output_info = helper.make_tensor_value_info(
        "score", TensorProto.FLOAT, ["batch", 1]
    )
    initializers = [
        numpy_helper.from_array(weights, name="W"),
        numpy_helper.from_array(bias, name="B"),
    ]
    nodes = [
        helper.make_node("Gemm", ["features", "W", "B"], ["logit"], alpha=1.0, beta=1.0),
        helper.make_node("Sigmoid", ["logit"], ["score"]),
    ]
    graph = helper.make_graph(
        nodes,
        f"ranking-{version}",
        [input_info],
        [output_info],
        initializer=initializers,
    )
    digest = hashlib.sha256(weights.tobytes() + bias.tobytes()).hexdigest()[:12]
    model = helper.make_model(
        graph,
        producer_name=f"ranking-{version}-{digest}",
        opset_imports=[helper.make_opsetid("", 13)],
    )
    model.doc_string = f"deterministic ranking model {version}"
    model.metadata_props.add(key="model_id", value=f"ranking-{version}-{digest}")
    model.metadata_props.add(key="protocol_version", value=protocol_version)
    model.metadata_props.add(key="seed", value=str(seed))
    model.ir_version = 8
    onnx.checker.check_model(model)
    return model


def main() -> None:
    parser = argparse.ArgumentParser(description="Export deterministic ranking ONNX models")
    parser.add_argument(
        "--output-dir", default="artifacts/models", help="directory for ONNX files"
    )
    parser.add_argument(
        "--check", action="store_true", help="only print deterministic model hashes"
    )
    args = parser.parse_args()

    output_dir = Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    result: dict[str, str] = {}
    for version, (protocol_version, seed, feature_count) in PROTOCOLS.items():
        model = build_model(version, protocol_version, seed, feature_count)
        payload = model.SerializeToString(deterministic=True)
        digest = hashlib.sha256(payload).hexdigest()
        result[version] = digest
        if not args.check:
            path = output_dir / f"ranking_{version}.onnx"
            path.write_bytes(payload)
            print(f"wrote {path} ({len(payload)} bytes, sha256={digest})")
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    main()
