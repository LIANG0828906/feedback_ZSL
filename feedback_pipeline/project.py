"""Project descriptor loading and validation.

The project JSON declares every channel file, the sample rate, the channel
count, channel names and roles (direct / feedback / reference) plus the
downmix and quality targets.  Any inconsistency aborts the whole pipeline
with a structured business error; no partial bus is ever emitted.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path

import soundfile as sf

from . import errors

VALID_ROLES = ("direct", "feedback", "reference")


@dataclass(frozen=True)
class ChannelSpec:
    name: str
    role: str
    file: str
    channels: int
    instrument: str = ""


@dataclass(frozen=True)
class Project:
    path: Path
    root: Path
    sample_rate: int
    duration_seconds: float
    seed: int
    song_segment: int
    channels: tuple[ChannelSpec, ...]
    downmix_weights: dict
    downmix_compensation: str
    loudness_target_lufs: float
    loudness_tolerance_lu: float
    true_peak_ceiling_dbtp: float
    allowed_false_positives: int
    min_attenuation_db: float
    block_size: int
    raw: dict = field(default_factory=dict)

    @property
    def reference_channels(self) -> list[ChannelSpec]:
        return [c for c in self.channels if c.role == "reference"]

    @property
    def mix_channels(self) -> list[ChannelSpec]:
        return [c for c in self.channels if c.role != "reference"]

    def channel_path(self, spec: ChannelSpec) -> Path:
        return self.root / spec.file


def _fail(code: str, message: str, channel: str | None = None):
    raise errors.PipelineError(code, message, channel)


def load_project(project_path: str | Path) -> Project:
    path = Path(project_path)
    if not path.is_file():
        _fail(errors.PROJECT_FILE_MISSING, f"project file not found: {path}")
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        _fail(errors.PROJECT_PARSE_ERROR, f"project file is not valid JSON: {exc}")
    if not isinstance(raw, dict):
        _fail(errors.PROJECT_SCHEMA_ERROR, "project root must be a JSON object")

    def need(key):
        if key not in raw:
            _fail(errors.PROJECT_SCHEMA_ERROR, f"missing required key: {key}")
        return raw[key]

    sample_rate = int(need("sample_rate"))
    duration = float(need("duration_seconds"))
    seed = int(need("seed"))
    segment = int(need("song_segment"))

    ch_entries = need("channels")
    if not isinstance(ch_entries, list) or not ch_entries:
        _fail(errors.PROJECT_SCHEMA_ERROR, "channels must be a non-empty list")

    root = path.parent
    specs: list[ChannelSpec] = []
    seen: set[str] = set()
    for entry in ch_entries:
        name = entry.get("name")
        if not name:
            _fail(errors.PROJECT_SCHEMA_ERROR, "channel entry missing 'name'")
        if name in seen:
            _fail(errors.DUPLICATE_CHANNEL_NAME, f"duplicate channel name: {name}", name)
        seen.add(name)
        role = entry.get("role")
        if role is None:
            _fail(errors.CHANNEL_ROLE_MISSING, f"channel '{name}' has no role", name)
        if role not in VALID_ROLES:
            _fail(errors.CHANNEL_ROLE_INVALID, f"channel '{name}' role '{role}' invalid", name)
        file_rel = entry.get("file")
        if not file_rel:
            _fail(errors.PROJECT_SCHEMA_ERROR, f"channel '{name}' missing 'file'", name)
        declared_ch = int(entry.get("channels", 1))
        file_path = root / file_rel
        if not file_path.is_file():
            _fail(errors.CHANNEL_FILE_MISSING, f"channel file not found: {file_rel}", name)
        info = sf.info(str(file_path))
        if info.samplerate != sample_rate:
            _fail(
                errors.SAMPLE_RATE_MISMATCH,
                f"channel '{name}' sample rate {info.samplerate} != project {sample_rate}",
                name,
            )
        if info.channels != declared_ch:
            _fail(
                errors.CHANNEL_COUNT_MISMATCH,
                f"channel '{name}' has {info.channels} channels, declared {declared_ch}",
                name,
            )
        specs.append(
            ChannelSpec(
                name=name,
                role=role,
                file=file_rel,
                channels=declared_ch,
                instrument=str(entry.get("instrument", "")),
            )
        )

    if not any(c.role == "reference" for c in specs):
        _fail(errors.REFERENCE_MISSING, "project declares no reference channel")

    downmix = need("downmix")
    weights = {str(k): float(v) for k, v in downmix.get("weights", {}).items()}
    compensation = str(downmix.get("compensation", "normalize"))
    if compensation not in ("normalize", "asis"):
        _fail(errors.PROJECT_SCHEMA_ERROR, f"unknown downmix compensation: {compensation}")
    for spec in specs:
        if spec.role != "reference" and spec.name not in weights:
            _fail(errors.PROJECT_SCHEMA_ERROR, f"downmix weight missing for channel '{spec.name}'", spec.name)

    return Project(
        path=path,
        root=root,
        sample_rate=sample_rate,
        duration_seconds=duration,
        seed=seed,
        song_segment=segment,
        channels=tuple(specs),
        downmix_weights=weights,
        downmix_compensation=compensation,
        loudness_target_lufs=float(need("loudness_target_lufs")),
        loudness_tolerance_lu=float(raw.get("loudness_tolerance_lu", 0.5)),
        true_peak_ceiling_dbtp=float(need("true_peak_ceiling_dbtp")),
        allowed_false_positives=int(raw.get("allowed_false_positives", 0)),
        min_attenuation_db=float(raw.get("min_attenuation_db", 20.0)),
        block_size=int(raw.get("block_size", 48000)),
        raw=raw,
    )


def project_digest(project: Project) -> str:
    """Content digest of the project JSON plus every channel file."""
    import hashlib

    h = hashlib.sha256()
    h.update(json.dumps(project.raw, sort_keys=True).encode("utf-8"))
    for spec in project.channels:
        h.update(spec.name.encode("utf-8"))
        file_path = project.channel_path(spec)
        h.update(hashlib.sha256(file_path.read_bytes()).digest())
    return h.hexdigest()
