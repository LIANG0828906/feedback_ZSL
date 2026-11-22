from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import E_DECODE, E_SCHEMA, SyncError


@dataclass(frozen=True)
class MarkerSpec:
    id: str
    kind: str
    ref_sample: int
    time_seconds: float
    template: dict[str, Any]


@dataclass(frozen=True)
class ChannelSpec:
    id: str
    file: str
    samples: int
    sample_rate: int
    content_sha256: str
    guard_before: int = 0
    guard_after: int = 0


@dataclass(frozen=True)
class ChannelLayout:
    name: str
    channels: tuple[str, ...]
    speakers: tuple[str, ...]


@dataclass(frozen=True)
class SessionManifest:
    schema_version: int
    session_id: str
    nominal_sample_rate: int
    channel_count: int
    reference_channel: str
    session_samples: int
    duration_seconds: float
    channels: tuple[ChannelSpec, ...]
    markers: tuple[MarkerSpec, ...]
    truth_file: str
    truth_sha256: str
    layout: ChannelLayout
    path: Path
    raw: dict[str, Any]

    def channel_ids(self) -> tuple[str, ...]:
        return tuple(channel.id for channel in self.channels)

    def marker_by_id(self, marker_id: str) -> MarkerSpec:
        for marker in self.markers:
            if marker.id == marker_id:
                return marker
        raise SyncError(E_SCHEMA, "unknown marker", marker=marker_id, field="markers")


@dataclass
class LoadedSession:
    manifest: SessionManifest
    channels: dict[str, np.ndarray]
    truth: dict[str, Any]
    truth_sha256: str

    @property
    def reference(self) -> np.ndarray:
        return self.channels[self.manifest.reference_channel]


def _require(container: dict[str, Any], field: str, *, channel: str | None = None) -> Any:
    if field not in container:
        raise SyncError(E_SCHEMA, f"missing required field '{field}'", channel=channel, field=field)
    return container[field]


def _as_int(value: Any, field: str, *, channel: str | None = None) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise SyncError(E_SCHEMA, f"field '{field}' must be an integer", channel=channel, field=field)
    return value


def _read_json(path: Path, *, label: str) -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
    except FileNotFoundError as exc:
        raise SyncError(E_SCHEMA, f"{label} file does not exist: {path}", field=label) from exc
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise SyncError(E_DECODE, f"cannot decode {label} file: {path}", field=label) from exc
    if not isinstance(value, dict):
        raise SyncError(E_SCHEMA, f"{label} root must be an object", field=label)
    return value


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as handle:
            for block in iter(lambda: handle.read(1024 * 1024), b""):
                digest.update(block)
    except OSError as exc:
        raise SyncError(E_DECODE, f"cannot read file: {path}", field=str(path)) from exc
    return digest.hexdigest()


def load_manifest(session_dir: str | Path, *, verify_truth: bool = True) -> SessionManifest:
    session_dir = Path(session_dir).resolve()
    manifest_path = session_dir / "manifest.json"
    raw = _read_json(manifest_path, label="manifest")

    schema_version = _as_int(_require(raw, "schema_version"), "schema_version")
    if schema_version != 1:
        raise SyncError(E_SCHEMA, f"unsupported manifest schema_version {schema_version}", field="schema_version")
    session_id = _require(raw, "session_id")
    if not isinstance(session_id, str) or not session_id:
        raise SyncError(E_SCHEMA, "session_id must be a non-empty string", field="session_id")
    nominal_sample_rate = _as_int(_require(raw, "nominal_sample_rate"), "nominal_sample_rate")
    if nominal_sample_rate <= 0:
        raise SyncError(E_SCHEMA, "nominal_sample_rate must be positive", field="nominal_sample_rate")
    channel_count = _as_int(_require(raw, "channel_count"), "channel_count")
    reference_channel = _require(raw, "reference_channel")
    if not isinstance(reference_channel, str) or not reference_channel:
        raise SyncError(E_SCHEMA, "reference_channel must be a non-empty string", field="reference_channel")
    session_samples = _as_int(_require(raw, "session_samples"), "session_samples")
    if session_samples <= 0:
        raise SyncError(E_SCHEMA, "session_samples must be positive", field="session_samples")
    duration_seconds = _require(raw, "duration_seconds")
    if not isinstance(duration_seconds, (int, float)) or duration_seconds <= 0:
        raise SyncError(E_SCHEMA, "duration_seconds must be positive", field="duration_seconds")
    expected_duration = session_samples / nominal_sample_rate
    if abs(float(duration_seconds) - expected_duration) > 1.0e-6:
        raise SyncError(
            E_SCHEMA,
            f"duration_seconds {duration_seconds} is inconsistent with session_samples/sample_rate={expected_duration}",
            field="duration_seconds",
        )

    raw_channels = _require(raw, "channels")
    if not isinstance(raw_channels, list) or not raw_channels:
        raise SyncError(E_SCHEMA, "channels must be a non-empty array", field="channels")
    if channel_count != len(raw_channels):
        raise SyncError(
            E_SCHEMA,
            f"channel_count={channel_count} does not match declared channels={len(raw_channels)}",
            field="channel_count",
        )

    channels: list[ChannelSpec] = []
    seen_ids: set[str] = set()
    for index, item in enumerate(raw_channels):
        if not isinstance(item, dict):
            raise SyncError(E_SCHEMA, "channel entry must be an object", field=f"channels[{index}]")
        channel_id = _require(item, "id")
        if not isinstance(channel_id, str) or not channel_id:
            raise SyncError(E_SCHEMA, "channel id must be a non-empty string", field="id")
        if channel_id in seen_ids:
            raise SyncError(E_SCHEMA, f"duplicate channel id '{channel_id}'", channel=channel_id, field="id")
        seen_ids.add(channel_id)
        file_name = _require(item, "file", channel=channel_id)
        if not isinstance(file_name, str) or not file_name:
            raise SyncError(E_SCHEMA, "channel file must be a non-empty string", channel=channel_id, field="file")
        samples = _as_int(_require(item, "samples", channel=channel_id), "samples", channel=channel_id)
        if samples <= 0:
            raise SyncError(E_SCHEMA, "samples must be positive", channel=channel_id, field="samples")
        channel_rate = _as_int(
            _require(item, "sample_rate", channel=channel_id), "sample_rate", channel=channel_id
        )
        if channel_rate != nominal_sample_rate:
            raise SyncError(
                E_SCHEMA,
                f"channel sample_rate {channel_rate} differs from nominal {nominal_sample_rate}",
                channel=channel_id,
                field="sample_rate",
            )
        content_sha256 = _require(item, "content_sha256", channel=channel_id)
        if not isinstance(content_sha256, str) or len(content_sha256) != 64:
            raise SyncError(
                E_SCHEMA,
                "content_sha256 must be a 64-character hex digest",
                channel=channel_id,
                field="content_sha256",
            )
        guard_before = _as_int(item.get("guard_before", 0), "guard_before", channel=channel_id)
        guard_after = _as_int(item.get("guard_after", 0), "guard_after", channel=channel_id)
        if guard_before < 0 or guard_after < 0:
            raise SyncError(
                E_SCHEMA,
                "guard samples must be non-negative",
                channel=channel_id,
                field="guard_before" if guard_before < 0 else "guard_after",
            )
        if channel_id == reference_channel and (
            guard_before != 0 or guard_after != 0 or samples != session_samples
        ):
            raise SyncError(
                E_SCHEMA,
                "reference channel must have exactly session_samples and zero guard",
                channel=channel_id,
                field="samples",
            )
        if samples < session_samples + guard_before + guard_after:
            raise SyncError(
                E_SCHEMA,
                "declared samples are smaller than session plus guard spans",
                channel=channel_id,
                field="samples",
            )
        channels.append(
            ChannelSpec(
                channel_id,
                file_name,
                samples,
                channel_rate,
                content_sha256,
                guard_before,
                guard_after,
            )
        )

    if reference_channel not in seen_ids:
        raise SyncError(
            E_SCHEMA,
            f"reference_channel '{reference_channel}' is not present in channels",
            field="reference_channel",
        )

    raw_markers = _require(raw, "markers")
    if not isinstance(raw_markers, list) or not raw_markers:
        raise SyncError(E_SCHEMA, "markers must be a non-empty array", field="markers")
    markers: list[MarkerSpec] = []
    seen_markers: set[str] = set()
    for index, item in enumerate(raw_markers):
        if not isinstance(item, dict):
            raise SyncError(E_SCHEMA, "marker entry must be an object", field=f"markers[{index}]")
        marker_id = _require(item, "id")
        if not isinstance(marker_id, str) or not marker_id:
            raise SyncError(E_SCHEMA, "marker id must be a non-empty string", field="id")
        if marker_id in seen_markers:
            raise SyncError(E_SCHEMA, f"duplicate marker id '{marker_id}'", field="id", marker=marker_id)
        seen_markers.add(marker_id)
        kind = _require(item, "kind", channel=None)
        if kind not in {"pulse", "chirp"}:
            raise SyncError(E_SCHEMA, "marker kind must be 'pulse' or 'chirp'", field="kind", marker=marker_id)
        ref_sample = _as_int(_require(item, "ref_sample"), "ref_sample", channel=None)
        if not 0 <= ref_sample < session_samples:
            raise SyncError(
                E_SCHEMA,
                "marker ref_sample is outside the session",
                field="ref_sample",
                marker=marker_id,
            )
        time_seconds = _require(item, "time_seconds")
        if not isinstance(time_seconds, (int, float)):
            raise SyncError(E_SCHEMA, "time_seconds must be numeric", field="time_seconds", marker=marker_id)
        if abs(float(time_seconds) - ref_sample / nominal_sample_rate) > 1.0 / nominal_sample_rate:
            raise SyncError(
                E_SCHEMA,
                "time_seconds is inconsistent with ref_sample",
                field="time_seconds",
                marker=marker_id,
            )
        template = _require(item, "template")
        if not isinstance(template, dict):
            raise SyncError(E_SCHEMA, "marker template must be an object", field="template", marker=marker_id)
        required_template = (
            ("carrier_hz", "sigma_samples")
            if kind == "pulse"
            else ("start_hz", "end_hz", "length_samples")
        )
        for field in ("sample_rate", *required_template):
            if field not in template:
                raise SyncError(
                    E_SCHEMA,
                    f"marker template is missing '{field}'",
                    field=f"template.{field}",
                    marker=marker_id,
                )
            value = template[field]
            if not isinstance(value, (int, float)) or value <= 0:
                raise SyncError(
                    E_SCHEMA,
                    f"marker template field '{field}' must be positive numeric",
                    field=f"template.{field}",
                    marker=marker_id,
                )
        if int(template["sample_rate"]) != nominal_sample_rate:
            raise SyncError(
                E_SCHEMA,
                "marker template sample_rate differs from session sample rate",
                field="template.sample_rate",
                marker=marker_id,
            )
        markers.append(MarkerSpec(marker_id, str(kind), ref_sample, float(time_seconds), template))

    truth = _require(raw, "truth")
    if not isinstance(truth, dict):
        raise SyncError(E_SCHEMA, "truth must be an object", field="truth")
    truth_file = _require(truth, "file")
    truth_sha256 = _require(truth, "sha256")
    if not isinstance(truth_file, str) or not truth_file:
        raise SyncError(E_SCHEMA, "truth.file must be a non-empty string", field="truth.file")
    if not isinstance(truth_sha256, str) or len(truth_sha256) != 64:
        raise SyncError(E_SCHEMA, "truth.sha256 must be a 64-character digest", field="truth.sha256")

    raw_layout = raw.get(
        "layout",
        {
            "name": "default",
            "channels": list(seen_ids),
            "speakers": [f"CH{i}" for i in range(len(seen_ids))],
        },
    )
    if not isinstance(raw_layout, dict):
        raise SyncError(E_SCHEMA, "layout must be an object", field="layout")
    layout_name = raw_layout.get("name", "default")
    layout_channels = raw_layout.get("channels")
    layout_speakers = raw_layout.get("speakers")
    if not isinstance(layout_name, str) or not isinstance(layout_channels, list) or not isinstance(
        layout_speakers, list
    ):
        raise SyncError(E_SCHEMA, "layout requires name, channels and speakers", field="layout")
    if len(layout_channels) != len(layout_speakers):
        raise SyncError(E_SCHEMA, "layout channels/speakers length mismatch", field="layout")
    if sorted(map(str, layout_channels)) != sorted(seen_ids):
        raise SyncError(E_SCHEMA, "layout must map every channel exactly once", field="layout.channels")

    manifest = SessionManifest(
        schema_version=schema_version,
        session_id=session_id,
        nominal_sample_rate=nominal_sample_rate,
        channel_count=channel_count,
        reference_channel=reference_channel,
        session_samples=session_samples,
        duration_seconds=float(duration_seconds),
        channels=tuple(channels),
        markers=tuple(markers),
        truth_file=truth_file,
        truth_sha256=truth_sha256,
        layout=ChannelLayout(layout_name, tuple(map(str, layout_channels)), tuple(map(str, layout_speakers))),
        path=session_dir,
        raw=raw,
    )

    if verify_truth:
        truth_path = (session_dir / truth_file).resolve()
        if not truth_path.exists():
            raise SyncError(E_SCHEMA, f"truth file does not exist: {truth_path}", field="truth.file")
        actual = sha256_file(truth_path)
        if actual != truth_sha256:
            raise SyncError(
                E_DECODE,
                "truth fingerprint mismatch",
                field="truth.sha256",
                details={"expected": truth_sha256, "actual": actual},
            )
    return manifest


def _validate_truth(manifest: SessionManifest, truth: dict[str, Any]) -> None:
    if truth.get("schema_version") != 1:
        raise SyncError(E_SCHEMA, "unsupported truth schema_version", field="truth.schema_version")
    if truth.get("session_id") != manifest.session_id:
        raise SyncError(E_SCHEMA, "truth session_id does not match manifest", field="truth.session_id")
    if truth.get("reference_channel") != manifest.reference_channel:
        raise SyncError(E_SCHEMA, "truth reference_channel does not match manifest", field="truth.reference_channel")
    if truth.get("sample_rate") != manifest.nominal_sample_rate:
        raise SyncError(E_SCHEMA, "truth sample_rate does not match manifest", field="truth.sample_rate")
    raw_channels = truth.get("channels")
    if not isinstance(raw_channels, dict):
        raise SyncError(E_SCHEMA, "truth.channels must be an object", field="truth.channels")
    for channel in manifest.channels:
        entry = raw_channels.get(channel.id)
        if not isinstance(entry, dict):
            raise SyncError(
                E_SCHEMA,
                "truth channel entry is missing",
                channel=channel.id,
                field="truth.channels",
            )
        for field in ("initial_offset_samples", "rates_ppm", "markers"):
            if field not in entry:
                raise SyncError(
                    E_SCHEMA,
                    f"truth channel is missing '{field}'",
                    channel=channel.id,
                    field=field,
                )
        marker_ids = {marker.id for marker in manifest.markers}
        truth_marker_ids = {
            item.get("id") for item in entry["markers"] if isinstance(item, dict) and isinstance(item.get("id"), str)
        }
        if marker_ids != truth_marker_ids:
            raise SyncError(
                E_SCHEMA,
                "truth marker ids do not match manifest",
                channel=channel.id,
                field="truth.markers",
            )


def load_session(session_dir: str | Path) -> LoadedSession:
    session_dir = Path(session_dir).resolve()
    manifest = load_manifest(session_dir, verify_truth=True)
    truth_path = (session_dir / manifest.truth_file).resolve()
    truth = _read_json(truth_path, label="truth")
    _validate_truth(manifest, truth)

    channels: dict[str, np.ndarray] = {}
    for spec in manifest.channels:
        channel_path = (session_dir / spec.file).resolve()
        try:
            data = np.load(channel_path, allow_pickle=False)
        except Exception as exc:
            raise SyncError(
                E_DECODE,
                f"cannot decode channel samples: {channel_path}",
                channel=spec.id,
                field="file",
            ) from exc
        if not isinstance(data, np.ndarray) or data.ndim != 1:
            raise SyncError(
                E_DECODE,
                "channel sample data must be a one-dimensional array",
                channel=spec.id,
                field="file",
            )
        if not np.issubdtype(data.dtype, np.number):
            raise SyncError(E_DECODE, "channel sample dtype is not numeric", channel=spec.id, field="file")
        if data.shape[0] != spec.samples:
            raise SyncError(
                E_SCHEMA,
                f"sample count {data.shape[0]} does not match manifest {spec.samples}",
                channel=spec.id,
                field="samples",
            )
        if not np.all(np.isfinite(data)):
            raise SyncError(E_DECODE, "channel contains non-finite samples", channel=spec.id, field="file")
        actual_hash = hashlib.sha256(np.ascontiguousarray(data).tobytes()).hexdigest()
        if actual_hash != spec.content_sha256:
            raise SyncError(
                E_DECODE,
                "channel content hash mismatch",
                channel=spec.id,
                field="content_sha256",
                details={"expected": spec.content_sha256, "actual": actual_hash},
            )
        channels[spec.id] = data

    if channels[manifest.reference_channel].shape[0] != manifest.session_samples:
        raise SyncError(
            E_SCHEMA,
            "reference channel sample count must equal session_samples",
            channel=manifest.reference_channel,
            field="samples",
        )
    return LoadedSession(manifest=manifest, channels=channels, truth=truth, truth_sha256=manifest.truth_sha256)


def validate_session(session_dir: str | Path) -> dict[str, Any]:
    loaded = load_session(session_dir)
    return {
        "ok": True,
        "session_id": loaded.manifest.session_id,
        "channel_count": loaded.manifest.channel_count,
        "sample_rate": loaded.manifest.nominal_sample_rate,
        "session_samples": loaded.manifest.session_samples,
        "markers": len(loaded.manifest.markers),
        "truth_sha256": loaded.truth_sha256,
    }

