from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path
from typing import Any, Dict, Mapping, Optional, Sequence, Tuple

import torch

from .errors import BusinessError, E_CKPT_CORRUPT, E_PARAM_SHAPE
from .tensor_codec import bytes_to_tensor
from .util import pretty_json_text


def _load_bundle(path: Path) -> Tuple[Dict[str, Any], Dict[str, torch.Tensor]]:
    index_path = path / "index.json"
    params_path = path / "params.bin"
    if not index_path.exists() or not params_path.exists():
        raise BusinessError(
            E_CKPT_CORRUPT,
            "final-state bundle is incomplete",
            {"path": str(path)},
        )
    try:
        index = json.loads(index_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise BusinessError(E_CKPT_CORRUPT, "final-state index is invalid JSON") from exc
    data = params_path.read_bytes()
    if hashlib.sha256(data).hexdigest() != index.get("sha256"):
        raise BusinessError(E_CKPT_CORRUPT, "final-state bundle hash mismatch")
    if len(data) != int(index.get("data_bytes", -1)):
        raise BusinessError(E_CKPT_CORRUPT, "final-state bundle size mismatch")
    tensors: Dict[str, torch.Tensor] = {}
    order = [str(name) for name in index["parameter_order"]]
    if [str(segment["name"]) for segment in index["segments"]] != order:
        raise BusinessError(E_PARAM_SHAPE, "final-state segment order is invalid")
    for segment in index["segments"]:
        name = str(segment["name"])
        offset = int(segment["offset"])
        nbytes = int(segment["nbytes"])
        expected_shape = tuple(int(v) for v in index["parameter_shapes"][name])
        if tuple(int(v) for v in segment["shape"]) != expected_shape:
            raise BusinessError(E_PARAM_SHAPE, "final-state tensor shape metadata mismatch")
        if offset < 0 or nbytes < 0 or offset + nbytes > len(data):
            raise BusinessError(E_PARAM_SHAPE, "final-state tensor segment is out of range")
        tensors[name] = bytes_to_tensor(data[offset : offset + nbytes], expected_shape)
    return index, tensors


def compare_bundles(
    server_path: str | Path,
    control_path: str | Path,
    atol: float = 1e-5,
) -> Dict[str, Any]:
    server_index, server_tensors = _load_bundle(Path(server_path))
    control_index, control_tensors = _load_bundle(Path(control_path))
    if server_index["layout_id"] != control_index["layout_id"]:
        raise BusinessError(
            E_PARAM_SHAPE,
            "server and control layouts differ",
            {
                "server": server_index["layout_id"],
                "control": control_index["layout_id"],
            },
        )
    if server_index["parameter_order"] != control_index["parameter_order"]:
        raise BusinessError(E_PARAM_SHAPE, "server and control parameter orders differ")
    max_abs_error = 0.0
    max_rel_error = 0.0
    squared_error = 0.0
    element_count = 0
    per_parameter: Dict[str, Dict[str, Any]] = {}
    for name in server_index["parameter_order"]:
        if tuple(server_tensors[name].shape) != tuple(control_tensors[name].shape):
            raise BusinessError(
                E_PARAM_SHAPE,
                "server and control parameter shapes differ",
                {"parameter": name},
            )
        difference = (server_tensors[name] - control_tensors[name]).abs()
        denominator = control_tensors[name].abs().clamp_min(1e-12)
        parameter_max = float(difference.max().item()) if difference.numel() else 0.0
        parameter_rel = float((difference / denominator).max().item()) if difference.numel() else 0.0
        max_abs_error = max(max_abs_error, parameter_max)
        max_rel_error = max(max_rel_error, parameter_rel)
        squared_error += float(torch.sum(difference.to(torch.float64) ** 2).item())
        element_count += int(difference.numel())
        per_parameter[name] = {
            "max_abs_error": parameter_max,
            "elements": int(difference.numel()),
        }
    rmse = (squared_error / element_count) ** 0.5 if element_count else 0.0
    return {
        "status": "PASS" if max_abs_error <= atol else "FAIL",
        "tolerance_abs": atol,
        "max_abs_error": max_abs_error,
        "max_rel_error": max_rel_error,
        "rmse": rmse,
        "element_count": element_count,
        "server_global_step": server_index["global_step"],
        "control_global_step": control_index["global_step"],
        "layout_id": server_index["layout_id"],
        "server_params_sha256": server_index["sha256"],
        "control_params_sha256": control_index["sha256"],
        "per_parameter": per_parameter,
    }


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = argparse.ArgumentParser(description="Compare server and control endpoint parameters")
    parser.add_argument("--server", required=True)
    parser.add_argument("--control", required=True)
    parser.add_argument("--atol", type=float, default=1e-5)
    parser.add_argument("--output")
    args = parser.parse_args(argv)
    try:
        result = compare_bundles(args.server, args.control, atol=args.atol)
        text = pretty_json_text(result)
        if args.output:
            Path(args.output).write_text(text, encoding="utf-8", newline="\n")
        print(text, end="")
        return 0 if result["status"] == "PASS" else 1
    except BusinessError as exc:
        print(pretty_json_text({"status": "ERROR", **exc.to_dict()}), file=sys.stderr, end="")
        return exc.exit_code


if __name__ == "__main__":
    raise SystemExit(main())
