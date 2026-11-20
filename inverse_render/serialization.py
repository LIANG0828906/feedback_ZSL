"""Fixed-field order serialization for render outputs, gradients, and traces."""

from __future__ import annotations

from collections import OrderedDict
from typing import Any, Mapping

import numpy as np
import torch

from .hashing import combine_hashes, tensor_content_hash


def tensor_to_ordered(tensor: torch.Tensor) -> OrderedDict[str, Any]:
    value = tensor.detach().cpu()
    data = value.tolist()
    return OrderedDict(
        (
            ("shape", list(value.shape)),
            ("dtype", str(value.dtype).replace("torch.", "")),
            ("values", data),
            ("content_hash", tensor_content_hash(value)),
        )
    )


def render_to_ordered(
    *,
    scene_name: str,
    resolution: tuple[int, int],
    nodes: Mapping[str, Mapping[str, torch.Tensor]],
    loss: torch.Tensor,
    extra: Mapping[str, Any] | None = None,
) -> OrderedDict[str, Any]:
    node_order = ("intersections", "visibility", "shadow_factor", "lighting", "loss")
    ordered_nodes: OrderedDict[str, Any] = OrderedDict()
    hashes: list[str] = []
    for node_name in node_order:
        payload = nodes[node_name]
        ordered_fields: OrderedDict[str, Any] = OrderedDict()
        for field_name, tensor in payload.items():
            ordered_fields[field_name] = tensor_to_ordered(tensor)
            hashes.append(ordered_fields[field_name]["content_hash"])
        ordered_nodes[node_name] = ordered_fields
    result: OrderedDict[str, Any] = OrderedDict(
        (
            ("schema_version", 1),
            ("scene", scene_name),
            ("resolution", [resolution[0], resolution[1]]),
            ("node_order", list(node_order)),
            ("nodes", ordered_nodes),
            ("loss", float(loss.detach().cpu().item())),
            ("loss_hash", tensor_content_hash(loss)),
            ("content_hash", combine_hashes(hashes)),
        )
    )
    if extra:
        result["extra"] = OrderedDict(extra.items())
    return result


def gradients_to_ordered(
    *,
    group_order: list[str],
    parameter_order: Mapping[str, list[str]],
    gradients: Mapping[str, Mapping[str, torch.Tensor]],
    loss: float,
) -> OrderedDict[str, Any]:
    groups: OrderedDict[str, Any] = OrderedDict()
    all_hashes: list[str] = []
    for group_id in group_order:
        params: OrderedDict[str, Any] = OrderedDict()
        squared = 0.0
        for path in parameter_order[group_id]:
            tensor = gradients[group_id][path]
            encoded = tensor_to_ordered(tensor)
            params[path] = encoded
            all_hashes.append(encoded["content_hash"])
            array = tensor.detach().cpu().numpy()
            squared += float(np.sum(np.square(array, dtype=np.float64)))
        groups[group_id] = OrderedDict(
            (
                ("parameters", params),
                ("gradient_norm", float(np.sqrt(squared))),
            )
        )
    return OrderedDict(
        (
            ("schema_version", 1),
            ("group_order", list(group_order)),
            ("groups", groups),
            ("loss", float(loss)),
            ("content_hash", combine_hashes(all_hashes)),
        )
    )


def trace_record(
    *,
    step: int,
    modified_groups: list[str],
    affected_nodes: int,
    reused_nodes: int,
    recomputed_nodes: int,
    loss: float,
    gradient_norm: float,
    content_hash: str,
    degraded: bool = False,
    degraded_reason: str | None = None,
    stale_nodes: list[str] | None = None,
    graph_version: str = "",
) -> OrderedDict[str, Any]:
    return OrderedDict(
        (
            ("step", int(step)),
            ("modified_groups", list(modified_groups)),
            ("affected_nodes", int(affected_nodes)),
            ("reused_nodes", int(reused_nodes)),
            ("recomputed_nodes", int(recomputed_nodes)),
            ("loss", float(loss)),
            ("gradient_norm", float(gradient_norm)),
            ("content_hash", str(content_hash)),
            ("graph_version", str(graph_version)),
            ("degraded", bool(degraded)),
            ("degraded_reason", degraded_reason),
            ("stale_nodes", list(stale_nodes or [])),
            ("ui_badge", "FULL_RECOMPUTE" if degraded else "OK"),
        )
    )

