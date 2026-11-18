from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


class ConfigError(Exception):
    def __init__(
        self,
        error_code: str,
        message: str,
        *,
        replica_id: str | None = None,
        field_name: str | None = None,
    ) -> None:
        super().__init__(message)
        self.error_code = error_code
        self.message = message
        self.replica_id = replica_id
        self.field_name = field_name

    def as_dict(self) -> dict[str, Any]:
        payload = {
            "ok": False,
            "error_code": self.error_code,
            "message": self.message,
        }
        if self.replica_id is not None:
            payload["replica_id"] = self.replica_id
        if self.field_name is not None:
            payload["field"] = self.field_name
        return payload


@dataclass(frozen=True)
class LayerSpec:
    layer_id: str
    name: str
    p95_threshold_ms: float
    call_timeout_ms: float
    depends_on: tuple[str, ...] = ()


@dataclass(frozen=True)
class ReplicaSpec:
    layer: str
    replica_group: str
    replica_id: str
    model_path: str
    listen_address: str
    input_dim: int
    output_dim: int


@dataclass(frozen=True)
class ServiceSpec:
    router_grpc: str
    http: str
    db_path: str
    request_timeout_ms: float
    l2_attempts: int
    thread_pool_size: int


@dataclass(frozen=True)
class HealthSpec:
    window_size: int
    min_samples: int
    probe_interval_ms: int
    probe_timeout_ms: int
    probe_failures_to_evict: int
    probe_successes_to_recover: int


@dataclass(frozen=True)
class StatsSpec:
    window_size: int
    p99_threshold_ms: float


@dataclass(frozen=True)
class RouterConfig:
    source_path: str
    layers: tuple[LayerSpec, ...]
    shards: tuple[ReplicaSpec, ...]
    service: ServiceSpec
    health: HealthSpec
    stats: StatsSpec
    raw: dict[str, Any] = field(repr=False)

    @property
    def layer_by_id(self) -> dict[str, LayerSpec]:
        return {layer.layer_id: layer for layer in self.layers}

    def replicas_by_layer(self) -> dict[str, list[ReplicaSpec]]:
        out = {layer.layer_id: [] for layer in self.layers}
        for replica in self.shards:
            out[replica.layer].append(replica)
        return out

    def fingerprint(self) -> str:
        canonical = json.dumps(self.raw, sort_keys=True, separators=(",", ":"))
        return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def _require(condition: bool, code: str, message: str, **kwargs: Any) -> None:
    if not condition:
        raise ConfigError(code, message, **kwargs)


def _as_mapping(value: Any, field_name: str) -> dict[str, Any]:
    _require(isinstance(value, dict), "CONFIG_SCHEMA_INVALID", f"{field_name} must be an object", field_name=field_name)
    return value


def _as_list(value: Any, field_name: str) -> list[Any]:
    _require(isinstance(value, list), "CONFIG_SCHEMA_INVALID", f"{field_name} must be an array", field_name=field_name)
    return value


def _positive_number(value: Any, field_name: str) -> float:
    _require(
        isinstance(value, (int, float)) and not isinstance(value, bool) and value > 0,
        "CONFIG_SCHEMA_INVALID",
        f"{field_name} must be a positive number",
        field_name=field_name,
    )
    return float(value)


def _positive_int(value: Any, field_name: str) -> int:
    _require(
        isinstance(value, int) and not isinstance(value, bool) and value > 0,
        "CONFIG_SCHEMA_INVALID",
        f"{field_name} must be a positive integer",
        field_name=field_name,
    )
    return value


def _string(value: Any, field_name: str) -> str:
    _require(
        isinstance(value, str) and bool(value.strip()),
        "CONFIG_SCHEMA_INVALID",
        f"{field_name} must be a non-empty string",
        field_name=field_name,
    )
    return value.strip()


def split_host_port(address: str, field_name: str = "listen_address") -> tuple[str, int]:
    try:
        host, port_text = address.rsplit(":", 1)
        port = int(port_text)
    except (ValueError, AttributeError) as exc:
        raise ConfigError(
            "CONFIG_SCHEMA_INVALID",
            f"{field_name} must be host:port",
            field_name=field_name,
        ) from exc
    _require(host != "" and 0 < port < 65536, "CONFIG_SCHEMA_INVALID", f"{field_name} has invalid host or port", field_name=field_name)
    return host, port


def _resolve_path(raw_path: str, base_dir: Path) -> str:
    path = Path(raw_path)
    if not path.is_absolute():
        path = base_dir / path
    return str(path.resolve())


def _topological_layers(layers: list[LayerSpec]) -> tuple[LayerSpec, ...]:
    by_id = {layer.layer_id: layer for layer in layers}
    visiting: set[str] = set()
    stack: list[str] = []
    visited: set[str] = set()
    ordered: list[LayerSpec] = []

    def visit(layer_id: str) -> None:
        if layer_id in visiting:
            start = stack.index(layer_id)
            cycle = " -> ".join([*stack[start:], layer_id])
            raise ConfigError(
                "CONFIG_LAYER_CYCLE",
                f"layer dependency cycle detected: {cycle}",
                field_name="layers.depends_on",
            )
        if layer_id in visited:
            return
        visiting.add(layer_id)
        stack.append(layer_id)
        layer = by_id[layer_id]
        for dependency in layer.depends_on:
            if dependency not in by_id:
                raise ConfigError(
                    "CONFIG_LAYER_UNKNOWN",
                    f"layer {layer_id} depends on unknown layer {dependency}",
                    field_name="layers.depends_on",
                )
            visit(dependency)
        visiting.remove(layer_id)
        stack.pop()
        visited.add(layer_id)
        ordered.append(layer)

    for layer in layers:
        visit(layer.layer_id)
    return tuple(ordered)


def _inspect_model(replica: ReplicaSpec) -> tuple[int, int]:
    if not os.path.isfile(replica.model_path):
        raise ConfigError(
            "CONFIG_MODEL_MISSING",
            f"model file does not exist: {replica.model_path}",
            replica_id=replica.replica_id,
            field_name="model_path",
        )
    try:
        import onnxruntime as ort

        options = ort.SessionOptions()
        options.intra_op_num_threads = 1
        options.inter_op_num_threads = 1
        options.log_severity_level = 3
        session = ort.InferenceSession(
            replica.model_path,
            sess_options=options,
            providers=["CPUExecutionProvider"],
        )
        inputs = session.get_inputs()
        outputs = session.get_outputs()
    except Exception as exc:
        raise ConfigError(
            "CONFIG_MODEL_INVALID",
            f"cannot load model file: {exc}",
            replica_id=replica.replica_id,
            field_name="model_path",
        ) from exc
    if len(inputs) != 1 or len(outputs) != 1:
        raise ConfigError(
            "CONFIG_DIMENSION_MISMATCH",
            "model must expose exactly one input and one output",
            replica_id=replica.replica_id,
            field_name="input_dim/output_dim",
        )

    def last_dimension(shape: Any) -> int | None:
        if not shape:
            return None
        value = shape[-1]
        return value if isinstance(value, int) else None

    actual_input = last_dimension(inputs[0].shape)
    actual_output = last_dimension(outputs[0].shape)
    if actual_input != replica.input_dim:
        raise ConfigError(
            "CONFIG_DIMENSION_MISMATCH",
            f"model input dimension {actual_input!r} != manifest {replica.input_dim}",
            replica_id=replica.replica_id,
            field_name="input_dim",
        )
    if actual_output != replica.output_dim:
        raise ConfigError(
            "CONFIG_DIMENSION_MISMATCH",
            f"model output dimension {actual_output!r} != manifest {replica.output_dim}",
            replica_id=replica.replica_id,
            field_name="output_dim",
        )
    return actual_input, actual_output


def load_router_config(path: str | Path, *, inspect_models: bool = True) -> RouterConfig:
    config_path = Path(path).resolve()
    if not config_path.is_file():
        raise ConfigError("CONFIG_FILE_NOT_FOUND", f"config file does not exist: {config_path}")
    try:
        raw = json.loads(config_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ConfigError("CONFIG_JSON_INVALID", f"cannot parse config JSON: {exc}") from exc
    data = _as_mapping(raw, "root")
    _require(data.get("schema_version") == 1, "CONFIG_SCHEMA_INVALID", "schema_version must be 1", field_name="schema_version")
    base_dir = config_path.parent

    layer_items = _as_list(data.get("layers"), "layers")
    _require(len(layer_items) > 0, "CONFIG_SCHEMA_INVALID", "layers must not be empty", field_name="layers")
    layers: list[LayerSpec] = []
    layer_ids: set[str] = set()
    for index, item in enumerate(layer_items):
        obj = _as_mapping(item, f"layers[{index}]")
        layer_id = _string(obj.get("layer_id"), f"layers[{index}].layer_id")
        _require(layer_id not in layer_ids, "CONFIG_DUPLICATE_LAYER", f"duplicate layer id: {layer_id}", field_name=f"layers[{index}].layer_id")
        layer_ids.add(layer_id)
        depends_on = _as_list(obj.get("depends_on", []), f"layers[{index}].depends_on")
        layers.append(
            LayerSpec(
                layer_id=layer_id,
                name=_string(obj.get("name", layer_id), f"layers[{index}].name"),
                p95_threshold_ms=_positive_number(obj.get("p95_threshold_ms"), f"layers[{index}].p95_threshold_ms"),
                call_timeout_ms=_positive_number(obj.get("call_timeout_ms"), f"layers[{index}].call_timeout_ms"),
                depends_on=tuple(
                    _string(value, f"layers[{index}].depends_on[{dep_index}]")
                    for dep_index, value in enumerate(depends_on)
                ),
            )
        )
    ordered_layers = _topological_layers(layers)

    shard_items = _as_list(data.get("shards"), "shards")
    _require(len(shard_items) > 0, "CONFIG_SCHEMA_INVALID", "shards must not be empty", field_name="shards")
    shards: list[ReplicaSpec] = []
    replica_ids: set[str] = set()
    listen_addresses: set[str] = set()
    group_layers: dict[str, str] = {}
    for index, item in enumerate(shard_items):
        obj = _as_mapping(item, f"shards[{index}]")
        prefix = f"shards[{index}]"
        layer = _string(obj.get("layer"), f"{prefix}.layer")
        replica_group = _string(obj.get("replica_group"), f"{prefix}.replica_group")
        replica_id = _string(obj.get("replica_id"), f"{prefix}.replica_id")
        if layer not in layer_ids:
            raise ConfigError(
                "CONFIG_LAYER_UNKNOWN",
                f"shard references unknown layer {layer}",
                replica_id=replica_id,
                field_name=f"{prefix}.layer",
            )
        _require(
            replica_id not in replica_ids,
            "CONFIG_DUPLICATE_REPLICA",
            f"duplicate replica id: {replica_id}",
            replica_id=replica_id,
            field_name=f"{prefix}.replica_id",
        )
        previous_layer = group_layers.get(replica_group)
        if previous_layer is not None and previous_layer != layer:
            raise ConfigError(
                "CONFIG_DUPLICATE_REPLICA_GROUP",
                f"replica group {replica_group} is reused by layers {previous_layer} and {layer}",
                replica_id=replica_id,
                field_name=f"{prefix}.replica_group",
            )
        group_layers[replica_group] = layer
        listen_address = _string(obj.get("listen_address"), f"{prefix}.listen_address")
        split_host_port(listen_address, f"{prefix}.listen_address")
        _require(
            listen_address not in listen_addresses,
            "CONFIG_DUPLICATE_LISTEN",
            f"duplicate listen address: {listen_address}",
            replica_id=replica_id,
            field_name=f"{prefix}.listen_address",
        )
        listen_addresses.add(listen_address)
        replica = ReplicaSpec(
            layer=layer,
            replica_group=replica_group,
            replica_id=replica_id,
            model_path=_resolve_path(_string(obj.get("model_path"), f"{prefix}.model_path"), base_dir),
            listen_address=listen_address,
            input_dim=_positive_int(obj.get("input_dim"), f"{prefix}.input_dim"),
            output_dim=_positive_int(obj.get("output_dim"), f"{prefix}.output_dim"),
        )
        if inspect_models:
            _inspect_model(replica)
        replica_ids.add(replica_id)
        shards.append(replica)

    shard_layers = {replica.layer for replica in shards}
    for layer in ordered_layers:
        if layer.layer_id not in shard_layers:
            raise ConfigError(
                "CONFIG_SCHEMA_INVALID",
                f"layer {layer.layer_id} has no replicas",
                field_name="shards",
            )
    replicas_by_layer: dict[str, list[ReplicaSpec]] = {}
    for replica in shards:
        replicas_by_layer.setdefault(replica.layer, []).append(replica)
    layer_dims: dict[str, tuple[int, int]] = {}
    for layer in ordered_layers:
        layer_replicas = replicas_by_layer[layer.layer_id]
        expected_input = layer_replicas[0].input_dim
        expected_output = layer_replicas[0].output_dim
        for replica in layer_replicas[1:]:
            if replica.input_dim != expected_input:
                raise ConfigError(
                    "CONFIG_DIMENSION_MISMATCH",
                    f"replica input_dim {replica.input_dim} != layer input_dim {expected_input}",
                    replica_id=replica.replica_id,
                    field_name="input_dim",
                )
            if replica.output_dim != expected_output:
                raise ConfigError(
                    "CONFIG_DIMENSION_MISMATCH",
                    f"replica output_dim {replica.output_dim} != layer output_dim {expected_output}",
                    replica_id=replica.replica_id,
                    field_name="output_dim",
                )
        layer_dims[layer.layer_id] = (expected_input, expected_output)
    for current, following in zip(ordered_layers, ordered_layers[1:]):
        if layer_dims[current.layer_id][1] != layer_dims[following.layer_id][0]:
            offending = replicas_by_layer[following.layer_id][0]
            raise ConfigError(
                "CONFIG_DIMENSION_MISMATCH",
                f"{current.layer_id} output dimension does not match "
                f"{following.layer_id} input dimension",
                replica_id=offending.replica_id,
                field_name="input_dim",
            )

    service_obj = _as_mapping(data.get("service"), "service")
    split_host_port(_string(service_obj.get("router_grpc"), "service.router_grpc"), "service.router_grpc")
    split_host_port(_string(service_obj.get("http"), "service.http"), "service.http")
    db_raw = _string(service_obj.get("db_path"), "service.db_path")
    service = ServiceSpec(
        router_grpc=_string(service_obj.get("router_grpc"), "service.router_grpc"),
        http=_string(service_obj.get("http"), "service.http"),
        db_path=_resolve_path(db_raw, base_dir),
        request_timeout_ms=_positive_number(service_obj.get("request_timeout_ms"), "service.request_timeout_ms"),
        l2_attempts=_positive_int(service_obj.get("l2_attempts", 2), "service.l2_attempts"),
        thread_pool_size=_positive_int(service_obj.get("thread_pool_size", 32), "service.thread_pool_size"),
    )

    health_obj = _as_mapping(data.get("health"), "health")
    health = HealthSpec(
        window_size=_positive_int(health_obj.get("window_size", 100), "health.window_size"),
        min_samples=_positive_int(health_obj.get("min_samples", 8), "health.min_samples"),
        probe_interval_ms=_positive_int(health_obj.get("probe_interval_ms", 500), "health.probe_interval_ms"),
        probe_timeout_ms=_positive_int(health_obj.get("probe_timeout_ms", 100), "health.probe_timeout_ms"),
        probe_failures_to_evict=_positive_int(
            health_obj.get("probe_failures_to_evict", 2), "health.probe_failures_to_evict"
        ),
        probe_successes_to_recover=_positive_int(
            health_obj.get("probe_successes_to_recover", 2), "health.probe_successes_to_recover"
        ),
    )
    stats_obj = _as_mapping(data.get("stats"), "stats")
    stats = StatsSpec(
        window_size=_positive_int(stats_obj.get("window_size", 300), "stats.window_size"),
        p99_threshold_ms=_positive_number(stats_obj.get("p99_threshold_ms", 250), "stats.p99_threshold_ms"),
    )
    return RouterConfig(
        source_path=str(config_path),
        layers=ordered_layers,
        shards=tuple(shards),
        service=service,
        health=health,
        stats=stats,
        raw=data,
    )


def load_worker_config(path: str | Path) -> dict[str, Any]:
    worker_path = Path(path).resolve()
    if not worker_path.is_file():
        raise ConfigError("CONFIG_FILE_NOT_FOUND", f"worker config does not exist: {worker_path}")
    try:
        data = json.loads(worker_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ConfigError("CONFIG_JSON_INVALID", f"cannot parse worker config JSON: {exc}") from exc
    obj = _as_mapping(data, "root")
    for field_name in (
        "layer",
        "replica_group",
        "replica_id",
        "listen_address",
    ):
        _string(obj.get(field_name), field_name)
    split_host_port(str(obj.get("listen_address")), "listen_address")
    obj["model_path"] = _resolve_path(_string(obj.get("model_path"), "model_path"), worker_path.parent)
    obj["input_dim"] = _positive_int(obj.get("input_dim"), "input_dim")
    obj["output_dim"] = _positive_int(obj.get("output_dim"), "output_dim")
    return obj
