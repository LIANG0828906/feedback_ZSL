from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from .config import Config
from .errors import E_META_INVALID, E_VERSION_LOSS, AppError
from .loudness import (
    LUFS_FLOOR,
    TRUE_PEAK_FLOOR,
    LoudnessAnalyzer,
    default_downmix_weights,
    round_db,
)
from .metadata import (
    CHECKSUM_ALGORITHM,
    BoundaryDiff,
    DerivedBoundaries,
    MetadataDocument,
    build_v3_document,
    compare_boundaries,
    derive_boundaries,
    parse_metadata,
)
from .util import canonical_json_bytes, sha256_bytes
from .wav import WavInfo


@dataclass(frozen=True)
class UpgradeResult:
    source_version: int
    document: MetadataDocument
    payload: dict[str, Any]
    field_mapping: tuple[dict[str, Any], ...]
    derived_boundaries: DerivedBoundaries
    boundary_diff: BoundaryDiff

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": True,
            "operation": "upgrade",
            "source_version": self.source_version,
            "target_version": 3,
            "document": self.payload,
            "field_mapping": list(self.field_mapping),
            "boundary_derivation": {
                "source": self.derived_boundaries.source,
                "rule": self.derived_boundaries.rule,
                "parameters": self.derived_boundaries.parameters,
            },
            "boundary_diff": self.boundary_diff.to_dict(),
        }


@dataclass(frozen=True)
class DowngradeResult:
    source_version: int
    target_version: int
    payload: dict[str, Any]
    loss_manifest: dict[str, Any]
    field_mapping: tuple[dict[str, Any], ...]
    parsed: MetadataDocument

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": not bool(self.loss_manifest["losses"]),
            "operation": "downgrade",
            "source_version": self.source_version,
            "target_version": self.target_version,
            "code": "E_VERSION_LOSS" if self.loss_manifest["losses"] else None,
            "document": self.payload,
            "loss_manifest": self.loss_manifest,
            "field_mapping": list(self.field_mapping),
        }


def _decision(
    decision_id: str,
    field: str,
    action: str,
    source: str,
    algorithm: str,
    inputs: dict[str, Any],
    result: Any,
    **extra: Any,
) -> dict[str, Any]:
    entry = {
        "decision_id": decision_id,
        "field": field,
        "action": action,
        "source": source,
        "algorithm": algorithm,
        "inputs": inputs,
        "result": result,
    }
    entry.update(extra)
    return entry


def _weight_hash(weights: tuple[tuple[str, float], ...]) -> str:
    return sha256_bytes(
        canonical_json_bytes(
            [{"channel": name, "weight": weight} for name, weight in weights]
        )
    )


def upgrade_to_v3(
    document: MetadataDocument,
    wav: WavInfo,
    config: Config,
) -> UpgradeResult:
    if document.version == 3:
        boundaries = derive_boundaries(document, config)
        mapping = (
            _decision(
                "v3-preserve-0",
                "$",
                "preserved",
                "v3 metadata",
                "strict parse and checksum verification",
                {"archive_checksum": document.archive_checksum},
                "unchanged",
            ),
        )
        return UpgradeResult(
            source_version=3,
            document=document,
            payload=document.raw,
            field_mapping=mapping,
            derived_boundaries=boundaries,
            boundary_diff=compare_boundaries(boundaries.segments, boundaries.segments),
        )
    derived = derive_boundaries(document, config)
    weights = default_downmix_weights(wav.channel_layout, wav.channel_names)
    analyzer = LoudnessAnalyzer(wav, config, weights)
    if document.version == 2:
        assert document.true_peak_limit_dbtp is not None
        limits: tuple[float, ...] | float = document.true_peak_limit_dbtp
        limit_source = "v2.true_peak_limit_dbtp"
        tolerance = config.quantization_tolerance_db
        tolerance_source = "config.quantization_tolerance_db"
        tolerance_action = "policy"
    else:
        limits = config.v1_true_peak_limit_dbtp
        limit_source = "config.v1_true_peak_limit_dbtp"
        tolerance = config.quantization_tolerance_db
        tolerance_source = "config.quantization_tolerance_db"
        tolerance_action = "policy"

    analyses = analyzer.analyze_boundaries(
        derived.segments,
        limits,
        boundary_source=derived.source,
    )
    segment_payload = []
    for analysis in analyses:
        segment_payload.append(
            {
                "index": analysis.index,
                "start_sample": analysis.start_sample,
                "end_sample": analysis.end_sample,
                "short_term_lufs": round_db(analysis.short_term_lufs),
                "true_peak_dbtp": round_db(analysis.true_peak_dbtp),
                "true_peak_limit_dbtp": round_db(analysis.true_peak_limit_dbtp),
                "over_limit": analysis.over_limit,
                "excess_db": round_db(analysis.excess_db),
            }
        )

    payload = build_v3_document(
        asset_id=document.asset_id,
        sample_rate=wav.sample_rate,
        duration_samples=wav.frames,
        integrated_lufs=document.integrated_lufs,
        target_offset_lu=document.target_offset_lu,
        channel_layout=wav.channel_layout,
        channel_mask=wav.channel_mask,
        downmix_weights=weights,
        true_peak_limit_dbtp=(
            document.true_peak_limit_dbtp
            if document.version == 2
            else config.v1_true_peak_limit_dbtp
        ),
        quantization_tolerance_db=tolerance,
        segments=segment_payload,
    )
    upgraded = parse_metadata(payload, wav=wav)
    wav_inputs = {
        "wav_file_sha256": wav.file_sha256,
        "channel_mask": f"0x{wav.channel_mask:04X}",
        "channel_layout": wav.channel_layout,
        "frames": wav.frames,
        "sample_rate": wav.sample_rate,
        "bits_per_sample": wav.bits_per_sample,
    }
    mapping: list[dict[str, Any]] = [
        _decision(
            "upgrade-000-preserve-integrated",
            "integrated_lufs",
            "preserved",
            f"v{document.version}.integrated_lufs",
            "copy existing value",
            {"value": document.integrated_lufs},
            document.integrated_lufs,
        ),
        _decision(
            "upgrade-001-preserve-offset",
            "target_offset_lu",
            "preserved",
            f"v{document.version}.target_offset_lu",
            "copy existing value",
            {"value": document.target_offset_lu},
            document.target_offset_lu,
        ),
        _decision(
            "upgrade-010-layout",
            "channel_layout",
            "derived",
            "wav_header.channel_mask",
            "decode RIFF channel mask to canonical layout",
            wav_inputs,
            wav.channel_layout,
        ),
        _decision(
            "upgrade-011-mask",
            "channel_mask",
            "derived",
            "wav_header.fmt.dwChannelMask",
            "copy validated channel mask",
            wav_inputs,
            wav.channel_mask,
        ),
        _decision(
            "upgrade-012-weights",
            "downmix_weights",
            "derived",
            "policy.default_downmix_for_layout",
            "mono:FC=1; 5.1:FL=FR=FC=1,LFE=0,BL=BR=1/sqrt(2)",
            {
                "layout": wav.channel_layout,
                "channel_names": list(wav.channel_names),
            },
            [{"channel": name, "weight": weight} for name, weight in weights],
        ),
        _decision(
            "upgrade-020-boundaries",
            "segments[].start_sample,end_sample",
            "derived",
            derived.source,
            derived.rule,
            derived.parameters,
            [
                {"index": index, "start_sample": start, "end_sample": end}
                for index, (start, end) in enumerate(derived.segments)
            ],
        ),
        _decision(
            "upgrade-021-limit",
            "segments[].true_peak_limit_dbtp",
            "preserved" if document.version == 2 else "policy",
            limit_source,
            "apply declared v2 limit" if document.version == 2 else "apply explicit v1 rework policy",
            {"limit_dbtp": document.true_peak_limit_dbtp if document.version == 2 else config.v1_true_peak_limit_dbtp},
            document.true_peak_limit_dbtp if document.version == 2 else config.v1_true_peak_limit_dbtp,
        ),
        _decision(
            "upgrade-022-tolerance",
            "quantization_tolerance_db",
            tolerance_action,
            tolerance_source,
            "copy configured quantization tolerance",
            {"config_fingerprint": config.fingerprint},
            tolerance,
        ),
        _decision(
            "upgrade-030-checksum",
            "archive_checksum",
            "generated",
            "canonical v3 document",
            CHECKSUM_ALGORITHM,
            {
                "canonicalization": "UTF-8 JSON, sorted keys, compact separators",
                "excluded_field": "archive_checksum",
            },
            payload["archive_checksum"],
        ),
    ]
    old_declared = {segment.index: segment for segment in document.segments}
    for analysis in analyses:
        previous = old_declared.get(analysis.index)
        entry_inputs = {
            "wav_file_sha256": wav.file_sha256,
            "start_sample": analysis.start_sample,
            "end_sample": analysis.end_sample,
            "short_term_window_samples": analysis.short_term_window_samples,
            "short_term_hop_samples": config.short_term_hop_samples,
            "downmix_weights_sha256": _weight_hash(weights),
            "analysis_algorithm_version": config.analysis_algorithm_version,
            "lufs_floor": LUFS_FLOOR,
            "true_peak_floor": TRUE_PEAK_FLOOR,
            "true_peak_oversample": config.true_peak_oversample,
        }
        extra = {}
        if previous is not None:
            extra = {
                "previous_declared": {
                    "short_term_lufs": previous.short_term_lufs,
                    "true_peak_dbtp": previous.true_peak_dbtp,
                    "over_limit": previous.over_limit,
                },
                "superseded_reason": "v2 lacks explicit boundaries; recomputed on derived boundaries",
            }
        mapping.append(
            _decision(
                f"upgrade-100-segment-{analysis.index}",
                f"segments[{analysis.index}].short_term_lufs,true_peak_dbtp,over_limit,excess_db",
                "recomputed",
                "wav_pcm + derived boundaries + downmix snapshot",
                config.analysis_algorithm_version,
                entry_inputs,
                {
                    "short_term_lufs": round_db(analysis.short_term_lufs),
                    "true_peak_dbtp": round_db(analysis.true_peak_dbtp),
                    "true_peak_limit_dbtp": round_db(analysis.true_peak_limit_dbtp),
                    "over_limit": analysis.over_limit,
                    "excess_db": round_db(analysis.excess_db),
                },
                **extra,
            )
        )
    return UpgradeResult(
        source_version=document.version,
        document=upgraded,
        payload=payload,
        field_mapping=tuple(mapping),
        derived_boundaries=derived,
        boundary_diff=compare_boundaries((), derived.segments),
    )


def _segment_loss_fields(target_version: int, limits_vary: bool) -> list[str]:
    common = ["start_sample", "end_sample"]
    if target_version == 1:
        return common + [
            "short_term_lufs",
            "true_peak_dbtp",
            "true_peak_limit_dbtp",
            "over_limit",
            "excess_db",
        ]
    fields = list(common)
    if limits_vary:
        fields.append("true_peak_limit_dbtp")
    return fields


def downgrade_document(
    document: MetadataDocument,
    target_version: int,
    wav: WavInfo,
    config: Config,
    *,
    allow_loss: bool = False,
) -> DowngradeResult:
    if target_version not in (1, 2, 3):
        raise AppError(
            E_META_INVALID,
            f"unknown target version {target_version}",
            field="target_version",
        )
    if document.version != 3:
        raise AppError(
            E_META_INVALID,
            "downgrade requires a v3 internal document",
            field="version",
        )
    if target_version == 3:
        parsed = parse_metadata(document.raw, wav=wav)
        return DowngradeResult(
            source_version=3,
            target_version=3,
            payload=document.raw,
            loss_manifest={
                "source_version": 3,
                "target_version": 3,
                "losses": [],
                "global_fields": [],
            },
            field_mapping=(),
            parsed=parsed,
        )

    segment_limit_values = {round_db(segment.true_peak_limit_dbtp) for segment in document.segments}
    limits_vary = len(segment_limit_values) > 1
    affected_fields = _segment_loss_fields(target_version, limits_vary)
    losses = [
        {
            "segment_index": segment.index,
            "fields": list(affected_fields),
        }
        for segment in document.segments
    ]
    global_fields = ["channel_layout", "channel_mask", "downmix_weights", "archive_checksum"]
    if target_version == 2:
        global_fields.append("quantization_tolerance_db")
    elif target_version == 1:
        global_fields.extend(
            [
                "quantization_tolerance_db",
                "true_peak_limit_dbtp",
                "segments",
            ]
        )

    mapping: list[dict[str, Any]] = []
    if target_version == 1:
        payload: dict[str, Any] = {
            "version": 1,
            "asset_id": document.asset_id,
            "sample_rate": document.sample_rate,
            "duration_samples": document.duration_samples,
            "integrated_lufs": round_db(document.integrated_lufs),
            "target_offset_lu": round_db(document.target_offset_lu),
        }
        mapping.append(
            _decision(
                "downgrade-000-preserve-track",
                "integrated_lufs,target_offset_lu",
                "preserved",
                "v3 document",
                "v1 can express track-level values",
                {"asset_id": document.asset_id},
                {
                    "integrated_lufs": payload["integrated_lufs"],
                    "target_offset_lu": payload["target_offset_lu"],
                },
            )
        )
    else:
        global_limit = round_db(
            document.true_peak_limit_dbtp
            if document.true_peak_limit_dbtp is not None
            else document.segments[0].true_peak_limit_dbtp
        )
        payload = {
            "version": 2,
            "asset_id": document.asset_id,
            "sample_rate": document.sample_rate,
            "duration_samples": document.duration_samples,
            "integrated_lufs": round_db(document.integrated_lufs),
            "target_offset_lu": round_db(document.target_offset_lu),
            "segment_count": len(document.segments),
            "true_peak_limit_dbtp": global_limit,
            "segments": [
                {
                    "index": segment.index,
                    "short_term_lufs": round_db(segment.short_term_lufs),
                    "true_peak_dbtp": round_db(segment.true_peak_dbtp),
                    "over_limit": segment.over_limit,
                }
                for segment in document.segments
            ],
        }
        mapping.append(
            _decision(
                "downgrade-000-track-and-segments",
                "integrated_lufs,target_offset_lu,segments[].short_term_lufs,true_peak_dbtp,over_limit",
                "preserved",
                "v3 document",
                "v2 can express segment values but not boundaries",
                {"asset_id": document.asset_id},
                {"segment_count": len(document.segments)},
            )
        )

    for field in global_fields:
        mapping.append(
            _decision(
                f"downgrade-drop-{field}",
                field,
                "dropped",
                "v3 document",
                f"target version v{target_version} has no field for this value",
                {
                    "source_present": field in document.raw,
                    "segments": [segment.index for segment in document.segments],
                },
                None,
            )
        )
    for segment in document.segments:
        for field in affected_fields:
            mapping.append(
                _decision(
                    f"downgrade-drop-segment-{segment.index}-{field}",
                    f"segments[{segment.index}].{field}",
                    "dropped",
                    "v3 document",
                    f"v{target_version} cannot independently restore this per-segment field",
                    {
                        "segment_index": segment.index,
                        "source_value": getattr(segment, field, None),
                    },
                    None,
                )
            )
    if target_version == 2:
        mapping.append(
            _decision(
                "downgrade-010-global-limit",
                "true_peak_limit_dbtp",
                "projected",
                "v3 document true_peak_limit_dbtp",
                "v2 supports one track-level limit",
                {"per_segment_limits_vary": limits_vary},
                document.true_peak_limit_dbtp,
            )
        )
    loss_manifest = {
        "source_version": 3,
        "target_version": target_version,
        "losses": losses,
        "global_fields": global_fields,
        "affected_segment_count": len(losses),
        "reversible": False,
        "mapping_table_independently_reproducible": True,
    }
    if losses and not allow_loss:
        raise AppError(
            E_VERSION_LOSS,
            f"downgrade to v{target_version} would lose non-expressible fields",
            field=f"target_version=v{target_version}",
            details=loss_manifest,
        )
    parsed = parse_metadata(payload, wav=wav)
    return DowngradeResult(
        source_version=3,
        target_version=target_version,
        payload=payload,
        loss_manifest=loss_manifest,
        field_mapping=tuple(mapping),
        parsed=parsed,
    )

