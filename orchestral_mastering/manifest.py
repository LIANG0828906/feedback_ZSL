from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import BusinessError, Issue, invalid

try:
    import soundfile as sf
except ImportError:  # pragma: no cover - dependency error is reported by the CLI
    sf = None


GROUP_ALIASES: dict[str, str] = {
    "弦乐": "弦乐",
    "弦乐组": "弦乐",
    "strings": "弦乐",
    "string": "弦乐",
    "木管": "木管",
    "木管组": "木管",
    "woodwind": "木管",
    "woodwinds": "木管",
    "铜管": "铜管",
    "铜管组": "铜管",
    "brass": "铜管",
    "打击乐": "打击乐",
    "打击乐组": "打击乐",
    "percussion": "打击乐",
    "timpani": "打击乐",
    "合唱": "合唱",
    "合唱组": "合唱",
    "choir": "合唱",
    "chorus": "合唱",
    "指挥提示": "指挥提示",
    "指挥": "指挥提示",
    "cue": "指挥提示",
    "conductor": "指挥提示",
    "conductor cue": "指挥提示",
}

REQUIRED_GROUPS = ("弦乐", "木管", "铜管", "打击乐", "合唱", "指挥提示")

SUBTYPE_BY_BITS = {
    16: "PCM_16",
    24: "PCM_24",
    32: "PCM_32",
}


@dataclass(frozen=True)
class TrackSpec:
    index: int
    name: str
    group: str
    role: str
    path: Path
    reference_path: Path
    anchor: tuple[float, float, float]
    target_energy_ratio: float
    onset_reference_seconds: float
    source_onset_seconds: float
    channels: int


@dataclass(frozen=True)
class MaskRelation:
    source_group: str
    target_group: str
    gain_db: float
    band_hz: tuple[float, float]
    windows_seconds: tuple[tuple[float, float], ...]


@dataclass(frozen=True)
class Manifest:
    path: Path
    raw_sha256: str
    schema_version: int
    seed: int
    sample_rate: int
    bit_depth: int
    channels: int
    duration_seconds: float
    block_frames: int
    target_loudness_lufs: float
    true_peak_max_dbtp: float
    unmask_windows_seconds: tuple[tuple[float, float], ...]
    residual_threshold_db: float
    residual_threshold_linear: float
    min_attenuation_db: float
    tracks: tuple[TrackSpec, ...]
    masking: tuple[MaskRelation, ...]
    raw: dict[str, Any]

    def track_by_role(self, role: str) -> TrackSpec:
        for track in self.tracks:
            if track.role == role:
                return track
        raise KeyError(role)

    def file_hashes(self) -> dict[str, str]:
        result: dict[str, str] = {}
        for track in self.tracks:
            result[f"{track.role}:stem"] = sha256_file(track.path)
            result[f"{track.role}:reference"] = sha256_file(track.reference_path)
        return result

    def input_digest(self) -> str:
        payload = {
            "manifest_sha256": self.raw_sha256,
            "files": self.file_hashes(),
        }
        return hashlib.sha256(
            json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ).hexdigest()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _number(value: Any, field_name: str, issues: list[Issue], *, finite: bool = True) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        issues.append(invalid(f"{field_name} 必须是数值", field=field_name))
        return math.nan
    result = float(value)
    if finite and not math.isfinite(result):
        issues.append(invalid(f"{field_name} 必须是有限数值", field=field_name))
    return result


def _integer(value: Any, field_name: str, issues: list[Issue]) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        issues.append(invalid(f"{field_name} 必须是整数", field=field_name))
        return 0
    return value


def _resolve(base: Path, value: Any) -> Path:
    path = Path(str(value))
    return path if path.is_absolute() else (base / path).resolve()


def _first(mapping: dict[str, Any], *names: str, default: Any = None) -> Any:
    for name in names:
        if name in mapping:
            return mapping[name]
    return default


def _parse_anchor(value: Any, issues: list[Issue], channel: str) -> tuple[float, float, float]:
    if isinstance(value, dict):
        parts = [value.get("x", 0.0), value.get("y", 0.0), value.get("z", 0.0)]
    elif isinstance(value, (list, tuple)) and len(value) == 3:
        parts = list(value)
    else:
        issues.append(invalid("anchor 必须包含 x/y/z 三个数值", channel=channel, group=channel))
        return (0.0, 0.0, 0.0)
    result: list[float] = []
    for axis, part in zip(("x", "y", "z"), parts):
        number = _number(part, f"anchor.{axis}", issues)
        result.append(number)
    return (result[0], result[1], result[2])


def _parse_windows(value: Any, issues: list[Issue], field_name: str) -> tuple[tuple[float, float], ...]:
    if not isinstance(value, list) or not value:
        issues.append(invalid(f"{field_name} 必须是非空区间数组", field=field_name))
        return ()
    windows: list[tuple[float, float]] = []
    for index, item in enumerate(value):
        if not isinstance(item, (list, tuple)) or len(item) != 2:
            issues.append(invalid(f"{field_name}[{index}] 必须是 [start,end]", field=field_name))
            continue
        start = _number(item[0], f"{field_name}[{index}].start", issues)
        end = _number(item[1], f"{field_name}[{index}].end", issues)
        if not end > start:
            issues.append(
                invalid(f"{field_name}[{index}] 的 end 必须大于 start", field=field_name)
            )
            continue
        windows.append((start, end))
    for previous, current in zip(windows, windows[1:]):
        if current[0] < previous[0]:
            issues.append(invalid(f"{field_name} 必须按开始时刻升序", field=field_name))
            break
    return tuple(windows)


def _parse_residual(unmasking: dict[str, Any], issues: list[Issue]) -> tuple[float, float]:
    if "residual_threshold_db" in unmasking:
        db = _number(unmasking["residual_threshold_db"], "unmasking.residual_threshold_db", issues)
        if db > 0:
            issues.append(invalid("unmasking.residual_threshold_db 必须不大于 0 dB"))
        return db, 10.0 ** (db / 20.0)
    value = _number(unmasking.get("residual_threshold"), "unmasking.residual_threshold", issues)
    if value < 0:
        return value, 10.0 ** (value / 20.0)
    if value > 1:
        issues.append(invalid("unmasking.residual_threshold 线性值必须位于 [0,1]"))
    return 20.0 * math.log10(max(value, 1e-12)), value


def _validate_audio_file(
    track: TrackSpec,
    declared_sample_rate: int,
    declared_bit_depth: int,
    issues: list[Issue],
) -> None:
    if sf is None:
        issues.append(
            Issue(
                "E_FORMAT_MISMATCH",
                "缺少 soundfile，无法核验音频格式",
                channel=track.name,
                group=track.group,
            )
        )
        return
    expected_subtype = SUBTYPE_BY_BITS.get(declared_bit_depth)
    for kind, path in (("stem", track.path), ("reference", track.reference_path)):
        if not path.is_file():
            issues.append(
                Issue(
                    "E_MISSING_FILE",
                    f"{kind} 文件不存在: {path}",
                    channel=track.name,
                    group=track.group,
                    details={"kind": kind, "path": str(path)},
                )
            )
            continue
        try:
            info = sf.info(str(path))
        except Exception as exc:
            issues.append(
                Issue(
                    "E_FORMAT_MISMATCH",
                    f"{kind} 文件无法读取: {exc}",
                    channel=track.name,
                    group=track.group,
                    details={"kind": kind, "path": str(path)},
                )
            )
            continue
        mismatches: dict[str, Any] = {}
        if str(info.format).upper() not in {"WAV", "WAVEX"}:
            mismatches["format"] = {"declared": "WAV", "actual": info.format}
        if info.samplerate != declared_sample_rate:
            mismatches["sample_rate"] = {
                "declared": declared_sample_rate,
                "actual": info.samplerate,
            }
        if info.channels != track.channels:
            mismatches["channels"] = {"declared": track.channels, "actual": info.channels}
        if expected_subtype is not None and info.subtype != expected_subtype:
            mismatches["subtype"] = {"declared": expected_subtype, "actual": info.subtype}
        if mismatches:
            issues.append(
                Issue(
                    "E_FORMAT_MISMATCH",
                    f"{kind} 音频格式与清单不一致",
                    channel=track.name,
                    group=track.group,
                    details={"kind": kind, "path": str(path), "mismatches": mismatches},
                )
            )


def load_manifest(path: str | Path, *, validate_files: bool = True) -> Manifest:
    manifest_path = Path(path).resolve()
    if not manifest_path.is_file():
        raise BusinessError(
            Issue(
                "E_MISSING_FILE",
                f"清单文件不存在: {manifest_path}",
                channel="manifest",
                details={"path": str(manifest_path)},
            )
        )
    raw_bytes = manifest_path.read_bytes()
    try:
        raw = json.loads(raw_bytes.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise BusinessError(
            Issue(
                "E_INVALID_MANIFEST",
                f"清单不是合法 UTF-8 JSON: {exc}",
                channel="manifest",
            )
        ) from exc
    if not isinstance(raw, dict):
        raise BusinessError(
            Issue("E_INVALID_MANIFEST", "清单根节点必须是对象", channel="manifest")
        )

    issues: list[Issue] = []
    schema_version = _integer(raw.get("schema_version", 1), "schema_version", issues)
    if schema_version != 1:
        issues.append(invalid("仅支持 schema_version=1", field="schema_version"))
    seed = _integer(raw.get("seed"), "seed", issues)
    sample_rate = _integer(
        _first(raw, "sample_rate", "sample_rate_hz", "samplerate"),
        "sample_rate",
        issues,
    )
    if sample_rate <= 0:
        issues.append(invalid("sample_rate 必须为正整数", field="sample_rate"))
    bit_depth = _integer(
        _first(raw, "bit_depth", "bit_depth_bits", "bits"),
        "bit_depth",
        issues,
    )
    if bit_depth not in SUBTYPE_BY_BITS:
        issues.append(invalid("bit_depth 仅支持 16/24/32 PCM", field="bit_depth"))
    channels = _integer(
        _first(raw, "channels", "channels_per_track", default=1),
        "channels",
        issues,
    )
    if channels != 1:
        issues.append(invalid("六路交响乐分轨必须为单声道", field="channels"))
    duration = _number(raw.get("duration_seconds", 8.0), "duration_seconds", issues)
    if duration <= 0:
        issues.append(invalid("duration_seconds 必须大于 0", field="duration_seconds"))
    block_frames = _integer(raw.get("block_frames", 16384), "block_frames", issues)
    if block_frames < 256:
        issues.append(invalid("block_frames 必须不小于 256", field="block_frames"))

    master = raw.get("master", {})
    if not isinstance(master, dict):
        issues.append(invalid("master 必须是对象", field="master"))
        master = {}
    target_loudness = _number(
        _first(
            master,
            "target_loudness_lufs",
            "loudness_lufs",
            default=_first(
                raw,
                "target_loudness_lufs",
                "target_master_loudness_lufs",
            ),
        ),
        "master.target_loudness_lufs",
        issues,
    )
    true_peak_max = _number(
        _first(
            master,
            "true_peak_max_dbtp",
            "true_peak_dbtp",
            "true_peak_limit_dbtp",
            default=_first(
                raw,
                "true_peak_max_dbtp",
                "true_peak_limit_dbtp",
                default=-1.0,
            ),
        ),
        "master.true_peak_max_dbtp",
        issues,
    )
    if true_peak_max > 0:
        issues.append(invalid("master.true_peak_max_dbtp 必须不大于 0 dBTP"))

    unmasking = _first(raw, "unmasking", "unmask", "decoupling", default={})
    if not isinstance(unmasking, dict):
        issues.append(invalid("unmasking 必须是对象", field="unmasking"))
        unmasking = {}
    unmask_windows = _parse_windows(
        _first(unmasking, "window_seconds", "windows_seconds", "time_windows"),
        issues,
        "unmasking.window_seconds",
    )
    residual_db, residual_linear = _parse_residual(unmasking, issues)
    min_attenuation_db = _number(
        _first(unmasking, "min_attenuation_db", "attenuation_db", default=12.0),
        "unmasking.min_attenuation_db",
        issues,
    )
    if min_attenuation_db <= 0:
        issues.append(invalid("unmasking.min_attenuation_db 必须大于 0", field="unmasking.min_attenuation_db"))

    tracks_raw = raw.get("tracks", raw.get("channels_data"))
    if not isinstance(tracks_raw, list) or len(tracks_raw) != 6:
        issues.append(invalid("tracks 必须包含恰好六路分轨", field="tracks"))
        tracks_raw = tracks_raw if isinstance(tracks_raw, list) else []

    tracks: list[TrackSpec] = []
    seen_groups: set[str] = set()
    seen_names: set[str] = set()
    base = manifest_path.parent
    for index, item in enumerate(tracks_raw):
        if not isinstance(item, dict):
            issues.append(invalid(f"tracks[{index}] 必须是对象", field="tracks"))
            continue
        name = str(_first(item, "name", "channel", "id", default=f"track_{index}")).strip()
        group_value = str(_first(item, "group", "group_name", default="")).strip()
        role = GROUP_ALIASES.get(group_value.lower()) or GROUP_ALIASES.get(group_value)
        if role is None:
            issues.append(
                invalid(
                    f"未知组名称: {group_value!r}",
                    channel=name,
                    group=group_value or None,
                    allowed_groups=list(REQUIRED_GROUPS),
                )
            )
            continue
        if role in seen_groups:
            issues.append(
                invalid(f"组名称重复: {group_value}", channel=name, group=group_value)
            )
        seen_groups.add(role)
        if name in seen_names:
            issues.append(
                invalid(f"通道名重复: {name}", channel=name, group=group_value)
            )
        seen_names.add(name)
        path_value = _first(item, "path", "file", "file_path")
        if not path_value:
            issues.append(invalid("缺少分轨文件路径", channel=name, group=role))
            path_value = f"__missing_track_{index}.wav"
        ref_value = _first(
            item,
            "reference_path",
            "monitor_reference",
            "monitor_reference_path",
            "listen_reference",
            "reference_channel",
            "reference",
        )
        if isinstance(ref_value, dict):
            ref_value = _first(ref_value, "path", "file")
        if not ref_value:
            issues.append(invalid("缺少独立监听参考通道路径", channel=name, group=role))
            ref_value = f"__missing_reference_{index}.wav"
        anchor = _parse_anchor(_first(item, "anchor", "stage_anchor", default={}), issues, name)
        ratio = _number(
            _first(item, "target_energy_ratio", "target_ratio", "energy_ratio"),
            "target_energy_ratio",
            issues,
        )
        if not (0.0 <= ratio <= 1.0):
            issues.append(
                invalid("目标声部能量比例必须位于 [0,1]", channel=name, group=role, value=ratio)
            )
        onset_reference = _number(
            _first(
                item,
                "onset_reference_seconds",
                "onset_reference",
                "reference_onset_seconds",
                "onset_reference_time",
            ),
            "onset_reference_seconds",
            issues,
        )
        if onset_reference < 0:
            issues.append(invalid("起音参考时刻不能为负", channel=name, group=role))
        source_onset = _number(
            _first(item, "source_onset_seconds", "synthetic_onset_seconds", default=onset_reference),
            "source_onset_seconds",
            issues,
        )
        if source_onset < 0:
            issues.append(invalid("合成源起音时刻不能为负", channel=name, group=role))
        track_channels = _integer(_first(item, "channels", default=channels), "channels", issues)
        if track_channels != channels:
            issues.append(
                invalid("各分轨声道数不一致", channel=name, group=role, expected=channels, actual=track_channels)
            )
        tracks.append(
            TrackSpec(
                index=index,
                name=name,
                group=role,
                role=role,
                path=_resolve(base, path_value),
                reference_path=_resolve(base, ref_value),
                anchor=anchor,
                target_energy_ratio=ratio,
                onset_reference_seconds=onset_reference,
                source_onset_seconds=source_onset,
                channels=track_channels,
            )
        )

    missing_groups = [group for group in REQUIRED_GROUPS if group not in seen_groups]
    if missing_groups:
        issues.append(
            invalid(
                f"缺少组名称: {', '.join(missing_groups)}",
                channel="manifest",
                details={"missing_groups": missing_groups},
            )
        )
    ratio_sum = sum(track.target_energy_ratio for track in tracks)
    if abs(ratio_sum - 1.0) > 1e-6:
        issues.append(
            invalid(
                "目标声部能量比例之和必须为 1",
                channel="manifest",
                details={"sum": ratio_sum},
            )
        )
    for previous, current in zip(tracks, tracks[1:]):
        if not current.onset_reference_seconds > previous.onset_reference_seconds:
            issues.append(
                invalid(
                    "起音参考时刻必须严格升序",
                    channel=current.name,
                    group=current.group,
                    details={
                        "previous": previous.onset_reference_seconds,
                        "current": current.onset_reference_seconds,
                    },
                )
            )

    masking_raw = _first(raw, "masking", "mask_relations", "injections", default=[])
    masking: list[MaskRelation] = []
    if not isinstance(masking_raw, list):
        issues.append(invalid("masking 必须是数组", field="masking"))
        masking_raw = []
    for index, item in enumerate(masking_raw):
        if not isinstance(item, dict):
            issues.append(invalid(f"masking[{index}] 必须是对象", field="masking"))
            continue
        source_value = str(_first(item, "source_group", "source", default="")).strip()
        target_value = str(_first(item, "target_group", "target", default="")).strip()
        source_role = GROUP_ALIASES.get(source_value.lower()) or GROUP_ALIASES.get(source_value)
        target_role = GROUP_ALIASES.get(target_value.lower()) or GROUP_ALIASES.get(target_value)
        if source_role not in REQUIRED_GROUPS or target_role not in REQUIRED_GROUPS:
            issues.append(
                invalid(
                    f"masking[{index}] 引用了未知组",
                    channel=target_value or f"masking_{index}",
                    details={"source_group": source_value, "target_group": target_value},
                )
            )
            continue
        band = _first(item, "band_hz", "band", default=[20.0, sample_rate / 2.0])
        if not isinstance(band, (list, tuple)) or len(band) != 2:
            issues.append(invalid(f"masking[{index}].band_hz 必须是 [low,high]"))
            continue
        low = _number(band[0], f"masking[{index}].band_hz.low", issues)
        high = _number(band[1], f"masking[{index}].band_hz.high", issues)
        if not (0 < low < high < sample_rate / 2.0):
            issues.append(invalid(f"masking[{index}].band_hz 必须位于 Nyquist 内", channel=target_value))
        windows_value = _first(
            item,
            "window_seconds",
            "windows_seconds",
            "time_windows",
            default=list(unmask_windows),
        )
        windows = _parse_windows(windows_value, issues, f"masking[{index}].window_seconds")
        gain_db = _number(
            _first(item, "gain_db", "injection_gain_db", default=-8.0),
            f"masking[{index}].gain_db",
            issues,
        )
        masking.append(
            MaskRelation(
                source_group=source_role,
                target_group=target_role,
                gain_db=gain_db,
                band_hz=(low, high),
                windows_seconds=windows,
            )
        )
    required_relations = {("铜管", "木管"), ("合唱", "弦乐")}
    present_relations = {(relation.source_group, relation.target_group) for relation in masking}
    if not required_relations.issubset(present_relations):
        missing = sorted(required_relations - present_relations)
        issues.append(
            invalid(
                "masking 必须声明铜管→木管与合唱→弦乐关系",
                channel="manifest",
                details={"missing": [list(item) for item in missing]},
            )
        )

    if validate_files:
        for track in tracks:
            _validate_audio_file(track, sample_rate, bit_depth, issues)

    if issues:
        raise BusinessError(issues)

    return Manifest(
        path=manifest_path,
        raw_sha256=hashlib.sha256(raw_bytes).hexdigest(),
        schema_version=schema_version,
        seed=seed,
        sample_rate=sample_rate,
        bit_depth=bit_depth,
        channels=channels,
        duration_seconds=duration,
        block_frames=block_frames,
        target_loudness_lufs=target_loudness,
        true_peak_max_dbtp=true_peak_max,
        unmask_windows_seconds=unmask_windows,
        residual_threshold_db=residual_db,
        residual_threshold_linear=residual_linear,
        min_attenuation_db=min_attenuation_db,
        tracks=tuple(tracks),
        masking=tuple(masking),
        raw=raw,
    )
