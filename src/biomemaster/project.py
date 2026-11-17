"""Project JSON parsing, validation, hashing, and loop loading."""

from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .constants import (
    CROSSFADE_SECONDS,
    MAX_WORKING_MEMORY_BYTES,
    MEMORY_FIXED_OVERHEAD_BYTES,
    MEMORY_PER_WORKER_BYTES,
    TARGET_LUFS,
)
from .errors import MemoryLimit, ProjectInvalid
from .model import Biome, Curve, Layer, LayerSpec, Project


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        while True:
            chunk = handle.read(1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()


def _reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ProjectInvalid(f"duplicate JSON key: {key}", field=key)
        result[key] = value
    return result


def _object(value: Any, field: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ProjectInvalid(f"{field} must be an object", field=field)
    return value


def _array(value: Any, field: str) -> list[Any]:
    if not isinstance(value, list):
        raise ProjectInvalid(f"{field} must be an array", field=field)
    return value


def _string(value: Any, field: str) -> str:
    if not isinstance(value, str) or not value:
        raise ProjectInvalid(f"{field} must be a non-empty string", field=field)
    return value


def _finite_number(value: Any, field: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ProjectInvalid(f"{field} must be a finite number", field=field)
    result = float(value)
    if not math.isfinite(result):
        raise ProjectInvalid(f"{field} must be a finite number", field=field)
    return result


def _integer(value: Any, field: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise ProjectInvalid(f"{field} must be an integer", field=field)
    return value


def _parse_curve(raw: Any, field: str) -> Curve:
    nodes = _array(raw, field)
    if not nodes:
        raise ProjectInvalid(f"{field} must contain at least one node", field=field)

    times: list[float] = []
    gains: list[float] = []
    previous = -math.inf
    for index, node in enumerate(nodes):
        node_field = f"{field}[{index}]"
        if isinstance(node, list):
            if len(node) != 2:
                raise ProjectInvalid(
                    f"{node_field} must contain [time, gain_db]", field=node_field
                )
            time_value, gain_value = node
        else:
            obj = _object(node, node_field)
            if "time" not in obj or ("gain_db" not in obj and "value" not in obj):
                raise ProjectInvalid(
                    f"{node_field} requires time and gain_db", field=node_field
                )
            time_value = obj["time"]
            gain_value = obj["gain_db"] if "gain_db" in obj else obj["value"]

        time_seconds = _finite_number(time_value, f"{node_field}.time")
        gain_db = _finite_number(gain_value, f"{node_field}.gain_db")
        if time_seconds < previous:
            raise ProjectInvalid(
                f"{field} node times must be non-decreasing",
                field=field,
                node_index=index,
                time_seconds=time_seconds,
                previous_time_seconds=previous,
            )
        if gain_db < -96.0 or gain_db > 12.0:
            raise ProjectInvalid(
                f"{node_field}.gain_db must be within [-96, 12]",
                field=node_field,
                gain_db=gain_db,
            )
        times.append(time_seconds)
        gains.append(gain_db)
        previous = time_seconds

    # Equal-time nodes are legal under the non-decreasing rule. Keep the last
    # one so interpolation is deterministic.
    unique_times: list[float] = []
    unique_gains: list[float] = []
    for time_seconds, gain_db in zip(times, gains):
        if unique_times and time_seconds == unique_times[-1]:
            unique_gains[-1] = gain_db
        else:
            unique_times.append(time_seconds)
            unique_gains.append(gain_db)
    return Curve(
        times_seconds=np.asarray(unique_times, dtype=np.float64),
        gains_db=np.asarray(unique_gains, dtype=np.float64),
    )


def _channel_layout(raw: Any) -> int:
    if raw == "stereo" or raw == 2:
        return 2
    raise ProjectInvalid(
        "output_channels must be 'stereo' or 2",
        field="output_channels",
        value=raw,
    )


def _parse_spec(
    raw: Any,
    biome_index: int,
    layer_index: int,
    base_dir: Path,
    sample_rate: int,
    crossfade_samples: int,
) -> LayerSpec:
    field = f"biomes[{biome_index}].layers[{layer_index}]"
    obj = _object(raw, field)
    path_text = _string(obj.get("path"), f"{field}.path")
    relative_path = path_text
    path = Path(path_text)
    if not path.is_absolute():
        path = base_dir / path
    path = path.resolve()

    if not path.is_file():
        raise ProjectInvalid(
            f"layer audio file does not exist: {path_text}",
            field=f"{field}.path",
            path=str(path),
        )

    loop_start = _integer(obj.get("loop_start"), f"{field}.loop_start")
    loop_end = _integer(obj.get("loop_end"), f"{field}.loop_end")
    if loop_start < 0 or loop_end <= loop_start:
        raise ProjectInvalid(
            "loop interval must satisfy 0 <= loop_start < loop_end",
            field=field,
            loop_start=loop_start,
            loop_end=loop_end,
        )

    base_gain_db = _finite_number(obj.get("base_gain_db", 0.0), f"{field}.base_gain_db")
    curve = _parse_curve(obj.get("curve"), f"{field}.curve")

    try:
        info = sf.info(str(path))
    except Exception as exc:
        raise ProjectInvalid(
            f"unable to read layer audio metadata: {path_text}",
            field=f"{field}.path",
            error=str(exc),
        ) from exc
    if info.samplerate != sample_rate:
        raise ProjectInvalid(
            f"layer sample rate {info.samplerate} does not match project {sample_rate}",
            field=f"{field}.path",
            expected_sample_rate=sample_rate,
            actual_sample_rate=info.samplerate,
        )
    if info.channels not in (1, 2):
        raise ProjectInvalid(
            "layer audio must be mono or stereo",
            field=f"{field}.path",
            channels=info.channels,
        )
    if loop_end > info.frames:
        raise ProjectInvalid(
            "loop_end exceeds the layer frame count",
            field=f"{field}.loop_end",
            loop_end=loop_end,
            frames=info.frames,
        )
    if loop_end - loop_start <= crossfade_samples:
        raise ProjectInvalid(
            "loop interval must be longer than the 20 ms equal-power crossfade",
            field=field,
            loop_samples=loop_end - loop_start,
            crossfade_samples=crossfade_samples,
        )

    name = obj.get("name")
    if name is None:
        name = Path(path_text).stem
    name = _string(name, f"{field}.name")
    return LayerSpec(
        name=name,
        path=path,
        relative_path=relative_path,
        loop_start=loop_start,
        loop_end=loop_end,
        base_gain_db=base_gain_db,
        curve=curve,
        source_channels=info.channels,
        source_frames=info.frames,
        source_sha256=sha256_file(path),
    )


def _validate_and_prepare(
    raw: dict[str, Any],
    source_path: Path,
    project_sha256: str,
    workers: int,
) -> Project:
    sample_rate = _integer(raw.get("sample_rate"), "sample_rate")
    if sample_rate < 8000 or sample_rate > 384000:
        raise ProjectInvalid(
            "sample_rate must be within [8000, 384000]",
            field="sample_rate",
            sample_rate=sample_rate,
        )
    duration_seconds = _finite_number(raw.get("duration_seconds"), "duration_seconds")
    if duration_seconds <= 0.0:
        raise ProjectInvalid(
            "duration_seconds must be positive",
            field="duration_seconds",
            duration_seconds=duration_seconds,
        )
    total_samples = int(math.floor(duration_seconds * sample_rate + 0.5))
    if total_samples <= 0:
        raise ProjectInvalid(
            "project duration rounds to zero samples",
            field="duration_seconds",
            duration_seconds=duration_seconds,
        )

    channels = _channel_layout(raw.get("output_channels"))
    target_loudness = _finite_number(
        raw.get("target_loudness_lufs", TARGET_LUFS), "target_loudness_lufs"
    )
    if target_loudness < -70.0 or target_loudness > 0.0:
        raise ProjectInvalid(
            "target_loudness_lufs must be within [-70, 0]",
            field="target_loudness_lufs",
            value=target_loudness,
        )

    crossfade_samples = int(round(CROSSFADE_SECONDS * sample_rate))
    biome_items = _array(raw.get("biomes"), "biomes")
    if not biome_items:
        raise ProjectInvalid("biomes must not be empty", field="biomes")

    biome_names: set[str] = set()
    layer_specs: list[tuple[str, float, tuple[LayerSpec, ...]]] = []
    content_items: list[dict[str, Any]] = []
    estimated_loop_bytes = 0

    for biome_index, biome_raw in enumerate(biome_items):
        biome_field = f"biomes[{biome_index}]"
        biome_obj = _object(biome_raw, biome_field)
        biome_name = _string(biome_obj.get("name"), f"{biome_field}.name")
        if biome_name in biome_names:
            raise ProjectInvalid(
                "biome names must be unique",
                field=f"{biome_field}.name",
                name=biome_name,
            )
        biome_names.add(biome_name)

        if "weight" in biome_obj and "weight_db" in biome_obj:
            raise ProjectInvalid(
                "specify only one of weight or weight_db",
                field=biome_field,
            )
        if "weight_db" in biome_obj:
            weight_db = _finite_number(
                biome_obj["weight_db"], f"{biome_field}.weight_db"
            )
            weight = 10.0 ** (weight_db / 20.0)
        else:
            weight = _finite_number(
                biome_obj.get("weight", 1.0), f"{biome_field}.weight"
            )
        if weight < 0.0 or weight > 1.0e6:
            raise ProjectInvalid(
                "biome weight must be finite, non-negative, and <= 1e6",
                field=f"{biome_field}.weight",
                weight=weight,
            )

        layer_items = _array(biome_obj.get("layers"), f"{biome_field}.layers")
        if not layer_items:
            raise ProjectInvalid(
                "each biome must contain at least one layer",
                field=f"{biome_field}.layers",
            )
        specs: list[LayerSpec] = []
        for layer_index, layer_raw in enumerate(layer_items):
            spec = _parse_spec(
                layer_raw,
                biome_index,
                layer_index,
                source_path.parent,
                sample_rate,
                crossfade_samples,
            )
            specs.append(spec)
            estimated_loop_bytes += spec.loop_samples * spec.source_channels * 8
            content_items.append(
                {
                    "path": spec.relative_path,
                    "loop_start": spec.loop_start,
                    "loop_end": spec.loop_end,
                    "sha256": spec.source_sha256,
                }
            )
        layer_specs.append((biome_name, weight, tuple(specs)))

    content_value = {
        "project_sha256": project_sha256,
        "layers": content_items,
    }
    content_sha256 = hashlib.sha256(canonical_json_bytes(content_value)).hexdigest()

    estimated_memory = (
        estimated_loop_bytes
        + MEMORY_FIXED_OVERHEAD_BYTES
        + max(1, workers) * MEMORY_PER_WORKER_BYTES
    )
    if estimated_memory > MAX_WORKING_MEMORY_BYTES:
        raise MemoryLimit(
            "estimated working set exceeds the 512 MB memory budget",
            estimated_bytes=estimated_memory,
            budget_bytes=MAX_WORKING_MEMORY_BYTES,
            loop_bytes=estimated_loop_bytes,
            workers=workers,
        )

    loaded_biomes: list[Biome] = []
    try:
        for biome_name, weight, specs in layer_specs:
            layers: list[Layer] = []
            for spec in specs:
                with sf.SoundFile(str(spec.path)) as handle:
                    handle.seek(spec.loop_start)
                    data = handle.read(
                        spec.loop_end - spec.loop_start,
                        dtype="float64",
                        always_2d=True,
                    )
                if data.shape[0] != spec.loop_samples:
                    raise ProjectInvalid(
                        "layer ended while reading its loop interval",
                        field=str(spec.path),
                    )
                if not np.all(np.isfinite(data)):
                    raise ProjectInvalid(
                        "layer loop contains non-finite samples",
                        field=str(spec.path),
                    )
                data = np.ascontiguousarray(data, dtype=np.float64)
                layers.append(Layer(spec=spec, loop_audio=data))
            loaded_biomes.append(
                Biome(name=biome_name, weight=weight, layers=tuple(layers))
            )
    except ProjectInvalid:
        raise
    except MemoryError as exc:
        raise MemoryLimit(
            "memory was exhausted while loading loop audio",
            estimated_bytes=estimated_memory,
            budget_bytes=MAX_WORKING_MEMORY_BYTES,
        ) from exc
    except Exception as exc:
        raise ProjectInvalid(
            "failed to load layer audio",
            error=str(exc),
        ) from exc

    return Project(
        source_path=source_path,
        project_sha256=project_sha256,
        content_sha256=content_sha256,
        sample_rate=sample_rate,
        duration_seconds=duration_seconds,
        total_samples=total_samples,
        channels=channels,
        target_loudness_lufs=target_loudness,
        biomes=tuple(loaded_biomes),
    )


def load_project(path: str | Path, *, workers: int = 1) -> Project:
    source_path = Path(path).expanduser().resolve()
    if not source_path.is_file():
        raise ProjectInvalid(
            "project JSON does not exist",
            field="project",
            path=str(source_path),
        )
    try:
        raw_bytes = source_path.read_bytes()
    except OSError as exc:
        raise ProjectInvalid(
            "project JSON is not readable", path=str(source_path), error=str(exc)
        ) from exc
    project_sha256 = hashlib.sha256(raw_bytes).hexdigest()
    try:
        raw = json.loads(
            raw_bytes.decode("utf-8"),
            object_pairs_hook=_reject_duplicate_keys,
        )
    except ProjectInvalid:
        raise
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ProjectInvalid(
            "project JSON is not valid UTF-8 JSON",
            error=str(exc),
        ) from exc
    root = _object(raw, "project")
    schema_version = root.get("schema_version", 1)
    if schema_version != 1:
        raise ProjectInvalid(
            "unsupported project schema_version",
            field="schema_version",
            value=schema_version,
        )
    return _validate_and_prepare(root, source_path, project_sha256, workers)
