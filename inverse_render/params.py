"""Parameter groups, versioning, and content-addressed parameter signatures."""

from __future__ import annotations

import fnmatch
import json
import re
from collections import OrderedDict
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Mapping

import numpy as np
import torch

from .errors import ErrorCode, ToolError, invalid_graph, invalid_scene
from .hashing import parameter_content_hash
from .scene import Scene


VALID_KINDS = ("camera", "geometry", "material", "light", "mixed")
_GROUP_ID_RE = re.compile(r"^[A-Za-z0-9_.-]+$")


@dataclass(frozen=True)
class GroupSpec:
    id: str
    kind: str
    parameters: tuple[str, ...]

    def to_ordered(self) -> OrderedDict[str, Any]:
        return OrderedDict(
            (
                ("id", self.id),
                ("kind", self.kind),
                ("parameters", list(self.parameters)),
            )
        )


def _read_group_json(path: str | Path) -> Any:
    group_path = Path(path).resolve()
    try:
        with open(group_path, "r", encoding="utf-8") as handle:
            return json.load(handle, object_pairs_hook=OrderedDict)
    except FileNotFoundError as exc:
        raise invalid_scene("group configuration does not exist", path=str(group_path)) from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise invalid_graph("group configuration is not valid JSON", path=str(group_path)) from exc


def default_group_specs(scene: Scene) -> list[GroupSpec]:
    return [
        GroupSpec("camera", "camera", tuple(
            path for path, kind in scene.parameter_kinds().items() if kind == "camera"
        )),
        GroupSpec("geometry", "geometry", tuple(
            path for path, kind in scene.parameter_kinds().items() if kind == "geometry"
        )),
        GroupSpec("material", "material", tuple(
            path for path, kind in scene.parameter_kinds().items() if kind == "material"
        )),
        GroupSpec("light", "light", tuple(
            path for path, kind in scene.parameter_kinds().items() if kind == "light"
        )),
    ]


def resolve_group_specs(
    scene: Scene,
    raw: Any | None = None,
) -> list[GroupSpec]:
    if raw is None:
        return default_group_specs(scene)
    if isinstance(raw, dict) and "groups" in raw:
        if raw.get("schema_version", 1) != 1:
            raise invalid_graph("unsupported group schema_version")
        entries = raw["groups"]
        if not isinstance(entries, list):
            raise invalid_graph("groups must be a list")
    elif isinstance(raw, list):
        entries = raw
    elif isinstance(raw, dict):
        entries = [
            {"id": group_id, "parameters": patterns}
            for group_id, patterns in raw.items()
        ]
    else:
        raise invalid_graph("group configuration must be an object or list")

    kinds = scene.parameter_kinds()
    all_paths = list(kinds)
    resolved: list[GroupSpec] = []
    seen_ids: set[str] = set()
    assigned: dict[str, str] = {}

    for index, entry in enumerate(entries):
        if not isinstance(entry, dict):
            raise invalid_graph(f"groups[{index}] must be an object")
        group_id = entry.get("id", entry.get("name"))
        if not isinstance(group_id, str) or not _GROUP_ID_RE.fullmatch(group_id):
            raise invalid_graph(
                f"groups[{index}].id is invalid",
                group_id=group_id,
            )
        if group_id in seen_ids:
            raise invalid_graph("duplicate group id", group_id=group_id)
        seen_ids.add(group_id)

        patterns = entry.get("parameters", entry.get("params"))
        if isinstance(patterns, str):
            patterns = [patterns]
        if not isinstance(patterns, list) or not patterns:
            raise invalid_graph("group must contain at least one parameter", group_id=group_id)
        if any(not isinstance(pattern, str) or not pattern for pattern in patterns):
            raise invalid_graph("group patterns must be non-empty strings", group_id=group_id)

        selected: list[str] = []
        for pattern in patterns:
            matches = [path for path in all_paths if fnmatch.fnmatchcase(path, pattern)]
            if not matches:
                raise invalid_graph(
                    "group pattern matches no parameter",
                    group_id=group_id,
                    pattern=pattern,
                )
            for match in matches:
                if match not in selected:
                    selected.append(match)
        selected = [path for path in all_paths if path in set(selected)]
        if not selected:
            raise invalid_graph("group is empty", group_id=group_id)

        selected_kinds = {kinds[path] for path in selected}
        kind = entry.get("kind")
        if kind is None:
            kind = next(iter(selected_kinds)) if len(selected_kinds) == 1 else "mixed"
        if kind not in VALID_KINDS:
            raise invalid_graph("invalid group kind", group_id=group_id, kind=kind)
        if kind != "mixed" and selected_kinds != {kind}:
            raise invalid_graph(
                "group kind does not match its parameters",
                group_id=group_id,
                kind=kind,
                parameter_kinds=sorted(selected_kinds),
            )

        for path in selected:
            if path in assigned:
                raise invalid_graph(
                    "parameter assigned to more than one group",
                    parameter=path,
                    groups=[assigned[path], group_id],
                )
            assigned[path] = group_id
        resolved.append(GroupSpec(group_id, str(kind), tuple(selected)))

    missing = [path for path in all_paths if path not in assigned]
    if missing:
        raise invalid_graph("some parameters are not assigned to a group", parameters=missing)
    return resolved


def load_group_specs(
    scene: Scene,
    path: str | Path | None = None,
) -> list[GroupSpec]:
    if path is not None:
        return resolve_group_specs(scene, _read_group_json(path))
    embedded = scene.raw.get("parameter_groups", scene.raw.get("groups"))
    if embedded is not None:
        return resolve_group_specs(scene, embedded)
    return default_group_specs(scene)


def group_specs_to_ordered(specs: Iterable[GroupSpec]) -> OrderedDict[str, Any]:
    return OrderedDict(
        (
            ("schema_version", 1),
            ("groups", [spec.to_ordered() for spec in specs]),
        )
    )


class ParameterStore:
    """Ordered differentiable parameters grouped by names supplied by the user."""

    def __init__(self, scene: Scene, groups: Iterable[GroupSpec]) -> None:
        self.scene = scene
        self.groups: OrderedDict[str, GroupSpec] = OrderedDict()
        for group in groups:
            self.groups[group.id] = group
        self.parameter_order: OrderedDict[str, list[str]] = OrderedDict(
            (group_id, list(group.parameters)) for group_id, group in self.groups.items()
        )
        self.parameters: OrderedDict[str, torch.nn.Parameter] = OrderedDict()
        initial = scene.initial_parameters()
        for path, value in initial.items():
            tensor = torch.as_tensor(np.array(value, copy=True), dtype=torch.float64)
            self.parameters[path] = torch.nn.Parameter(tensor, requires_grad=True)
        self._path_group: dict[str, str] = {}
        for group_id, group in self.groups.items():
            for path in group.parameters:
                if path not in self.parameters:
                    raise invalid_graph("group refers to unknown parameter", path=path)
                self._path_group[path] = group_id
        if set(self._path_group) != set(self.parameters):
            raise invalid_graph("parameter group coverage is incomplete")
        self._versions: OrderedDict[str, int] = OrderedDict(
            (group_id, 1) for group_id in self.groups
        )

    @classmethod
    def from_checkpoint(
        cls,
        scene: Scene,
        groups: Iterable[GroupSpec],
        values: Mapping[str, Mapping[str, Any]],
    ) -> "ParameterStore":
        store = cls(scene, groups)
        store.apply_checkpoint_groups(values)
        return store

    @classmethod
    def clone_fresh(cls, source: "ParameterStore") -> "ParameterStore":
        clone = cls.__new__(cls)
        clone.scene = source.scene
        clone.groups = OrderedDict(source.groups)
        clone.parameter_order = OrderedDict(
            (group_id, list(paths)) for group_id, paths in source.parameter_order.items()
        )
        clone.parameters = OrderedDict()
        for path, parameter in source.parameters.items():
            clone.parameters[path] = torch.nn.Parameter(
                parameter.detach().clone(), requires_grad=True
            )
        clone._path_group = dict(source._path_group)
        clone._versions = OrderedDict(source._versions)
        return clone

    @property
    def group_order(self) -> list[str]:
        return list(self.groups)

    def versions(self) -> OrderedDict[str, int]:
        return OrderedDict(self._versions)

    def parameter_content_hashes(self) -> OrderedDict[str, str]:
        return OrderedDict(
            (path, parameter_content_hash(parameter))
            for path, parameter in self.parameters.items()
        )

    def group_content_hashes(self) -> OrderedDict[str, str]:
        all_hashes = self.parameter_content_hashes()
        result: OrderedDict[str, str] = OrderedDict()
        for group_id, paths in self.parameter_order.items():
            digest = "\n".join(f"{path}:{all_hashes[path]}" for path in paths)
            result[group_id] = parameter_content_hash(
                torch.tensor(list(digest.encode("utf-8")), dtype=torch.uint8)
            )
        return result

    def signatures(
        self, group_ids: Iterable[str] | None = None
    ) -> OrderedDict[str, OrderedDict[str, Any]]:
        selected = list(group_ids) if group_ids is not None else self.group_order
        result: OrderedDict[str, OrderedDict[str, Any]] = OrderedDict()
        for group_id in selected:
            hashes: OrderedDict[str, str] = OrderedDict()
            for path in self.parameter_order[group_id]:
                hashes[path] = parameter_content_hash(self.parameters[path])
            result[group_id] = OrderedDict(
                (
                    ("version", self._versions[group_id]),
                    ("parameter_hashes", hashes),
                )
            )
        return result

    def group_values(self, group_id: str) -> OrderedDict[str, np.ndarray]:
        if group_id not in self.groups:
            raise invalid_graph("unknown parameter group", group_id=group_id)
        return OrderedDict(
            (
                path,
                self.parameters[path].detach().cpu().numpy().copy(),
            )
            for path in self.parameter_order[group_id]
        )

    def all_group_values(self) -> OrderedDict[str, OrderedDict[str, np.ndarray]]:
        return OrderedDict(
            (group_id, self.group_values(group_id)) for group_id in self.group_order
        )

    def checkpoint_groups(self) -> OrderedDict[str, Any]:
        result: OrderedDict[str, Any] = OrderedDict()
        for group_id in self.group_order:
            group_values: OrderedDict[str, Any] = OrderedDict()
            for path, array in self.group_values(group_id).items():
                if array.ndim == 0:
                    group_values[path] = float(array.item())
                else:
                    group_values[path] = array.tolist()
            result[group_id] = group_values
        return result

    def update_group(
        self,
        group_id: str,
        values: Mapping[str, Any],
        *,
        increment_version: bool = True,
    ) -> None:
        if group_id not in self.groups:
            raise invalid_graph("unknown parameter group", group_id=group_id)
        allowed = set(self.parameter_order[group_id])
        unknown = set(values) - allowed
        if unknown:
            raise invalid_graph(
                "update contains parameters outside the group",
                group_id=group_id,
                parameters=sorted(unknown),
            )
        if not values:
            raise invalid_graph("update must contain at least one parameter", group_id=group_id)
        with torch.no_grad():
            for path, value in values.items():
                parameter = self.parameters[path]
                tensor = torch.as_tensor(np.asarray(value), dtype=torch.float64)
                if tensor.shape != parameter.shape:
                    raise invalid_graph(
                        "parameter update shape mismatch",
                        group_id=group_id,
                        parameter=path,
                        expected=list(parameter.shape),
                        actual=list(tensor.shape),
                    )
                if not torch.isfinite(tensor).all():
                    raise invalid_graph(
                        "parameter update contains non-finite values",
                        group_id=group_id,
                        parameter=path,
                    )
                parameter.copy_(tensor)
        if increment_version:
            self._versions[group_id] += 1

    def set_parameter(
        self,
        path: str,
        value: Any,
        *,
        increment_version: bool = True,
    ) -> None:
        if path not in self.parameters:
            raise invalid_graph("unknown parameter", path=path)
        group_id = self._path_group[path]
        self.update_group(
            group_id,
            {path: value},
            increment_version=increment_version,
        )

    def apply_checkpoint_groups(
        self,
        values: Mapping[str, Mapping[str, Any]],
        *,
        increment_versions: bool = False,
    ) -> None:
        expected_groups = set(self.group_order)
        actual_groups = set(values)
        if actual_groups != expected_groups:
            raise invalid_graph(
                "checkpoint group set does not match configuration",
                missing=sorted(expected_groups - actual_groups),
                extra=sorted(actual_groups - expected_groups),
            )
        for group_id in self.group_order:
            group_values = values[group_id]
            if not isinstance(group_values, dict):
                raise invalid_graph("checkpoint group value must be an object", group_id=group_id)
            expected_paths = set(self.parameter_order[group_id])
            actual_paths = set(group_values)
            if actual_paths != expected_paths:
                raise invalid_graph(
                    "checkpoint parameter set does not match group",
                    group_id=group_id,
                    missing=sorted(expected_paths - actual_paths),
                    extra=sorted(actual_paths - expected_paths),
                )
            self.update_group(
                group_id,
                group_values,
                increment_version=increment_versions,
            )

    def zero_grad(self) -> None:
        for parameter in self.parameters.values():
            parameter.grad = None

    def group_gradients(self) -> OrderedDict[str, OrderedDict[str, torch.Tensor]]:
        result: OrderedDict[str, OrderedDict[str, torch.Tensor]] = OrderedDict()
        for group_id in self.group_order:
            gradients: OrderedDict[str, torch.Tensor] = OrderedDict()
            for path in self.parameter_order[group_id]:
                grad = self.parameters[path].grad
                if grad is None:
                    grad = torch.zeros_like(self.parameters[path])
                gradients[path] = grad.detach().clone()
            result[group_id] = gradients
        return result

    def gradient_norm(self) -> float:
        total = 0.0
        for parameter in self.parameters.values():
            if parameter.grad is not None:
                array = parameter.grad.detach().cpu().numpy()
                total += float(np.sum(np.square(array, dtype=np.float64)))
        return float(np.sqrt(total))

    def mark_group_modified(self, group_id: str) -> None:
        if group_id not in self.groups:
            raise invalid_graph("unknown parameter group", group_id=group_id)
        self._versions[group_id] += 1

    def optimizer_step(self, group_id: str, learning_rate: float) -> None:
        """Plain deterministic SGD update followed by one version increment."""

        if group_id not in self.groups:
            raise invalid_graph("unknown parameter group", group_id=group_id)
        if learning_rate <= 0.0:
            raise ToolError(
                ErrorCode.E_CONFIG_INVALID,
                "learning_rate must be positive",
                details={"learning_rate": learning_rate},
            )
        with torch.no_grad():
            for path in self.parameter_order[group_id]:
                parameter = self.parameters[path]
                if parameter.grad is not None:
                    parameter.add_(parameter.grad, alpha=-learning_rate)
        self._versions[group_id] += 1

