from __future__ import annotations

import math
from dataclasses import dataclass
from types import MappingProxyType
from typing import Any, Iterable, Mapping

from .canonical import normalize_value, sha256_obj
from .errors import material_invalid, pipeline_uninit


SUPPORTED_TARGETS = ("srgb", "display-p3")
SUPPORTED_OBSERVERS = ("2deg", "10deg")
SUPPORTED_ADAPTATIONS = ("bradford", "von-kries", "none")


def freeze_value(value: Any) -> Any:
    if isinstance(value, Mapping):
        return MappingProxyType({str(k): freeze_value(v) for k, v in value.items()})
    if isinstance(value, (list, tuple)):
        return tuple(freeze_value(v) for v in value)
    return value


def thaw_value(value: Any) -> Any:
    if isinstance(value, Mapping):
        return {str(k): thaw_value(v) for k, v in value.items()}
    if isinstance(value, tuple):
        return [thaw_value(v) for v in value]
    return value


def _as_float(value: Any, path: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise material_invalid(path, "must be a finite number")
    result = float(value)
    if not math.isfinite(result):
        raise material_invalid(path, "must be finite")
    return result


def _as_xyz(value: Any, path: str) -> tuple[float, float, float]:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise material_invalid(path, "must contain exactly three XYZ values")
    xyz = tuple(_as_float(item, f"{path}[{index}]") for index, item in enumerate(value))
    if any(component <= 0.0 for component in xyz):
        raise material_invalid(path, "XYZ components must be positive")
    return xyz  # type: ignore[return-value]


@dataclass(frozen=True)
class GridSpec:
    start_nm: float
    end_nm: float
    step_nm: float

    def __post_init__(self) -> None:
        if not all(
            isinstance(value, (int, float)) and not isinstance(value, bool)
            for value in (self.start_nm, self.end_nm, self.step_nm)
        ):
            raise material_invalid("pipeline.grid", "grid values must be numbers")
        if not all(
            math.isfinite(float(value))
            for value in (self.start_nm, self.end_nm, self.step_nm)
        ):
            raise material_invalid("pipeline.grid", "grid values must be finite")
        if float(self.step_nm) <= 0:
            raise material_invalid("pipeline.grid.step_nm", "must be greater than zero")
        if float(self.end_nm) <= float(self.start_nm):
            raise material_invalid("pipeline.grid.end_nm", "must be greater than start_nm")
        span = float(self.end_nm) - float(self.start_nm)
        steps = span / float(self.step_nm)
        if abs(steps - round(steps)) > 1e-9:
            raise material_invalid(
                "pipeline.grid.step_nm", "must evenly divide the wavelength range"
            )

    @property
    def sample_count(self) -> int:
        return int(round((self.end_nm - self.start_nm) / self.step_nm)) + 1

    def wavelengths(self) -> tuple[float, ...]:
        return tuple(
            round(self.start_nm + index * self.step_nm, 10)
            for index in range(self.sample_count)
        )

    def to_dict(self) -> dict[str, float]:
        return {
            "start_nm": self.start_nm,
            "end_nm": self.end_nm,
            "step_nm": self.step_nm,
        }

    @classmethod
    def from_dict(cls, value: Mapping[str, Any], path: str = "pipeline.grid") -> "GridSpec":
        if not isinstance(value, Mapping):
            raise material_invalid(path, "must be an object")
        for key in ("start_nm", "end_nm", "step_nm"):
            if key not in value:
                raise material_invalid(f"{path}.{key}", "required field is missing")
        return cls(
            _as_float(value["start_nm"], f"{path}.start_nm"),
            _as_float(value["end_nm"], f"{path}.end_nm"),
            _as_float(value["step_nm"], f"{path}.step_nm"),
        )


@dataclass(frozen=True)
class IlluminantSpec:
    name: str
    temperature_k: float | None = None
    wavelengths_nm: tuple[float, ...] | None = None
    values: tuple[float, ...] | None = None

    def __post_init__(self) -> None:
        if not isinstance(self.name, str) or not self.name.strip():
            raise material_invalid("pipeline.illuminant.name", "must be a non-empty string")
        if self.temperature_k is not None:
            if not math.isfinite(float(self.temperature_k)) or float(self.temperature_k) <= 0:
                raise material_invalid(
                    "pipeline.illuminant.temperature_k", "must be positive and finite"
                )
        supplied = self.wavelengths_nm is not None or self.values is not None
        if supplied:
            if self.wavelengths_nm is None or self.values is None:
                raise material_invalid(
                    "pipeline.illuminant",
                    "wavelengths_nm and values must be supplied together",
                )
            if len(self.wavelengths_nm) < 2:
                raise material_invalid(
                    "pipeline.illuminant.wavelengths_nm",
                    "must contain at least two samples",
                )
            if len(self.wavelengths_nm) != len(self.values):
                raise material_invalid(
                    "pipeline.illuminant.values",
                    "length must match wavelengths_nm",
                )
            previous = -math.inf
            for index, wavelength in enumerate(self.wavelengths_nm):
                if not math.isfinite(float(wavelength)):
                    raise material_invalid(
                        f"pipeline.illuminant.wavelengths_nm[{index}]", "must be finite"
                    )
                if float(wavelength) <= previous:
                    raise material_invalid(
                        f"pipeline.illuminant.wavelengths_nm[{index}]",
                        "wavelengths must be strictly increasing",
                    )
                previous = float(wavelength)
            for index, value in enumerate(self.values):
                if not math.isfinite(float(value)) or float(value) < 0:
                    raise material_invalid(
                        f"pipeline.illuminant.values[{index}]",
                        "must be finite and non-negative",
                    )

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "temperature_k": self.temperature_k,
            "wavelengths_nm": self.wavelengths_nm,
            "values": self.values,
        }

    @classmethod
    def from_dict(
        cls, value: Mapping[str, Any], path: str = "pipeline.illuminant"
    ) -> "IlluminantSpec":
        if not isinstance(value, Mapping):
            raise material_invalid(path, "must be an object")
        if "name" not in value:
            raise material_invalid(f"{path}.name", "required field is missing")
        wavelengths = value.get("wavelengths_nm", value.get("wavelengths"))
        values = value.get("values")
        wavelengths_tuple = None
        values_tuple = None
        if wavelengths is not None:
            if not isinstance(wavelengths, (list, tuple)):
                raise material_invalid(f"{path}.wavelengths_nm", "must be an array")
            wavelengths_tuple = tuple(
                _as_float(item, f"{path}.wavelengths_nm[{index}]")
                for index, item in enumerate(wavelengths)
            )
        if values is not None:
            if not isinstance(values, (list, tuple)):
                raise material_invalid(f"{path}.values", "must be an array")
            values_tuple = tuple(
                _as_float(item, f"{path}.values[{index}]")
                for index, item in enumerate(values)
            )
        temperature = value.get("temperature_k")
        temperature_float = (
            _as_float(temperature, f"{path}.temperature_k")
            if temperature is not None
            else None
        )
        return cls(
            name=str(value["name"]),
            temperature_k=temperature_float,
            wavelengths_nm=wavelengths_tuple,
            values=values_tuple,
        )


@dataclass(frozen=True)
class PipelineConfig:
    grid: GridSpec
    observer: str
    adaptation: str
    white_point_name: str
    white_point_xyz: tuple[float, float, float]
    illuminant: IlluminantSpec
    targets: tuple[str, ...] = SUPPORTED_TARGETS

    def __post_init__(self) -> None:
        if self.observer not in SUPPORTED_OBSERVERS:
            raise pipeline_uninit(
                "pipeline.observer", f"unsupported observer {self.observer!r}"
            )
        if self.adaptation not in SUPPORTED_ADAPTATIONS:
            raise pipeline_uninit(
                "pipeline.adaptation", f"unsupported adaptation {self.adaptation!r}"
            )
        if not self.white_point_name:
            raise material_invalid("pipeline.white_point.name", "must not be empty")
        _as_xyz(self.white_point_xyz, "pipeline.white_point.xyz")
        if not self.targets:
            raise material_invalid("pipeline.targets", "must not be empty")
        unknown = [target for target in self.targets if target not in SUPPORTED_TARGETS]
        if unknown:
            raise material_invalid(
                "pipeline.targets", f"unsupported target color space {unknown[0]!r}"
            )

    @property
    def target_tuple(self) -> tuple[str, ...]:
        requested = set(self.targets)
        return tuple(target for target in SUPPORTED_TARGETS if target in requested)

    def to_dict(self) -> dict[str, Any]:
        return {
            "grid": self.grid.to_dict(),
            "observer": self.observer,
            "adaptation": self.adaptation,
            "white_point": {
                "name": self.white_point_name,
                "xyz": self.white_point_xyz,
            },
            "illuminant": self.illuminant.to_dict(),
            "targets": self.targets,
        }

    @classmethod
    def from_dict(
        cls, value: Mapping[str, Any], path: str = "pipeline"
    ) -> "PipelineConfig":
        if not isinstance(value, Mapping):
            raise material_invalid(path, "must be an object")
        for key in (
            "grid",
            "observer",
            "adaptation",
            "white_point",
            "illuminant",
        ):
            if key not in value:
                raise material_invalid(f"{path}.{key}", "required field is missing")
        white_point = value["white_point"]
        if not isinstance(white_point, Mapping):
            raise material_invalid(f"{path}.white_point", "must be an object")
        xyz_value = white_point.get("xyz")
        if xyz_value is None:
            raise material_invalid(
                f"{path}.white_point.xyz", "required field is missing"
            )
        return cls(
            grid=GridSpec.from_dict(value["grid"], f"{path}.grid"),
            observer=str(value["observer"]),
            adaptation=str(value["adaptation"]),
            white_point_name=str(white_point.get("name", "")),
            white_point_xyz=_as_xyz(xyz_value, f"{path}.white_point.xyz"),
            illuminant=IlluminantSpec.from_dict(
                value["illuminant"], f"{path}.illuminant"
            ),
            targets=tuple(str(item) for item in value.get("targets", SUPPORTED_TARGETS)),
        )

    def config_hash(self) -> str:
        return sha256_obj(self.to_dict())


@dataclass(frozen=True)
class VersionRecord:
    material_id: str
    version: int
    parent_version: int | None
    params: Mapping[str, Any]
    dependencies: Mapping[str, Any]
    colors: Mapping[str, Any]
    intermediates: Mapping[str, Any]
    content_hash: str
    chain_hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "material_id": self.material_id,
            "version": self.version,
            "parent_version": self.parent_version,
            "params": thaw_value(self.params),
            "dependencies": thaw_value(self.dependencies),
            "colors": thaw_value(self.colors),
            "intermediates": thaw_value(self.intermediates),
            "content_hash": self.content_hash,
            "chain_hash": self.chain_hash,
        }

    @classmethod
    def from_dict(cls, value: Mapping[str, Any]) -> "VersionRecord":
        return cls(
            material_id=str(value["material_id"]),
            version=int(value["version"]),
            parent_version=(
                None if value.get("parent_version") is None else int(value["parent_version"])
            ),
            params=freeze_value(value["params"]),
            dependencies=freeze_value(value["dependencies"]),
            colors=freeze_value(value["colors"]),
            intermediates=freeze_value(value["intermediates"]),
            content_hash=str(value["content_hash"]),
            chain_hash=str(value["chain_hash"]),
        )


@dataclass(frozen=True)
class MaterialHistory:
    material_id: str
    versions: tuple[VersionRecord, ...]

    def current(self) -> VersionRecord:
        return self.versions[-1]

    def get(self, version: int) -> VersionRecord:
        for record in self.versions:
            if record.version == version:
                return record
        raise KeyError(version)


@dataclass(frozen=True)
class PublicationChainEntry:
    generation: int
    previous_publication_hash: str
    publication_hash: str
    pipeline_hash: str
    material_versions: Mapping[str, int]
    record_hashes: Mapping[str, str]
    chain_hashes: Mapping[str, str]

    def hash_payload(self) -> dict[str, Any]:
        return {
            "generation": self.generation,
            "previous_publication_hash": self.previous_publication_hash,
            "pipeline_hash": self.pipeline_hash,
            "material_versions": thaw_value(self.material_versions),
            "record_hashes": thaw_value(self.record_hashes),
            "chain_hashes": thaw_value(self.chain_hashes),
        }

    def to_dict(self) -> dict[str, Any]:
        return {
            **self.hash_payload(),
            "publication_hash": self.publication_hash,
        }

    @classmethod
    def from_dict(cls, value: Mapping[str, Any]) -> "PublicationChainEntry":
        return cls(
            generation=int(value["generation"]),
            previous_publication_hash=str(value.get("previous_publication_hash", "")),
            publication_hash=str(value["publication_hash"]),
            pipeline_hash=str(value["pipeline_hash"]),
            material_versions=freeze_value(value["material_versions"]),
            record_hashes=freeze_value(value["record_hashes"]),
            chain_hashes=freeze_value(value["chain_hashes"]),
        )


@dataclass(frozen=True)
class PublishedSnapshot:
    library_id: str
    seed: int
    generation: int
    previous_generation: int
    publication_hash: str
    previous_publication_hash: str
    pipeline: PipelineConfig
    histories: Mapping[str, MaterialHistory]
    publication_chain: tuple[PublicationChainEntry, ...]
    ui_state: Mapping[str, Any]

    def current_record(self, material_id: str) -> VersionRecord:
        return self.histories[material_id].current()

    def current_materials(self) -> dict[str, dict[str, Any]]:
        result: dict[str, dict[str, Any]] = {}
        for material_id in sorted(self.histories):
            record = self.current_record(material_id)
            result[material_id] = {
                "version": record.version,
                "colors": thaw_value(record.colors),
                "content_hash": record.content_hash,
                "chain_hash": record.chain_hash,
                "dependencies": thaw_value(record.dependencies),
            }
        return result


@dataclass(frozen=True)
class PendingJob:
    library_id: str
    base_generation: int
    base_publication_hash: str
    operation: str
    requested_pipeline: PipelineConfig
    material_update: Mapping[str, Any] | None = None
    rollback: Mapping[str, Any] | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema": "spectral-editor-pending/v1",
            "library_id": self.library_id,
            "base_generation": self.base_generation,
            "base_publication_hash": self.base_publication_hash,
            "operation": self.operation,
            "requested_pipeline": self.requested_pipeline.to_dict(),
            "material_update": thaw_value(self.material_update),
            "rollback": thaw_value(self.rollback),
        }

    @classmethod
    def from_dict(cls, value: Mapping[str, Any]) -> "PendingJob":
        return cls(
            library_id=str(value["library_id"]),
            base_generation=int(value["base_generation"]),
            base_publication_hash=str(value["base_publication_hash"]),
            operation=str(value["operation"]),
            requested_pipeline=PipelineConfig.from_dict(
                value["requested_pipeline"], "pending.requested_pipeline"
            ),
            material_update=(
                freeze_value(value["material_update"])
                if value.get("material_update") is not None
                else None
            ),
            rollback=(
                freeze_value(value["rollback"])
                if value.get("rollback") is not None
                else None
            ),
        )


@dataclass(frozen=True)
class TransitionRequest:
    operation: str
    pipeline: PipelineConfig | None = None
    material_update: Mapping[str, Any] | None = None
    rollback: Mapping[str, Any] | None = None


def deduplicate_sorted(values: Iterable[str]) -> tuple[str, ...]:
    return tuple(sorted(set(values)))
