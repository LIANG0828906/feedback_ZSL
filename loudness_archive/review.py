from __future__ import annotations

import itertools
from pathlib import Path
from typing import Any

from .config import Config
from .errors import E_SEGMENT_MISMATCH
from .loudness import (
    LUFS_FLOOR,
    TRUE_PEAK_FLOOR,
    LoudnessAnalyzer,
    SegmentAnalysis,
    default_downmix_weights,
    round_db,
)
from .metadata import (
    BoundaryDiff,
    MetadataDocument,
    compare_boundaries,
    derive_boundaries,
)
from .util import canonical_json_bytes, sha256_bytes
from .wav import WavInfo

_FIELD_ORDER = (
    "start_sample",
    "end_sample",
    "short_term_lufs",
    "true_peak_dbtp",
    "true_peak_limit_dbtp",
    "over_limit",
)


def _values_equal(field: str, left: Any, right: Any, tolerance: float) -> bool:
    if field in {"start_sample", "end_sample", "over_limit"}:
        return left == right
    if left is None or right is None:
        return left is right
    return abs(float(left) - float(right)) <= tolerance + 1e-12


def _analysis_for(
    document: MetadataDocument,
    wav: WavInfo,
    config: Config,
    tolerance: float,
) -> tuple[dict[str, Any], tuple[SegmentAnalysis, ...], BoundaryDiff]:
    derived = derive_boundaries(document, config)
    weights = (
        document.downmix_weights
        if document.version == 3 and document.downmix_weights is not None
        else default_downmix_weights(wav.channel_layout, wav.channel_names)
    )
    analyzer = LoudnessAnalyzer(wav, config, weights)
    if document.version == 3:
        limits = document.limits()
        limit_source = "v3 segments[].true_peak_limit_dbtp"
    elif document.version == 2:
        assert document.true_peak_limit_dbtp is not None
        limits = (document.true_peak_limit_dbtp,) * len(derived.segments)
        limit_source = "v2 true_peak_limit_dbtp"
    else:
        limits = (config.v1_true_peak_limit_dbtp,) * len(derived.segments)
        limit_source = "config.v1_true_peak_limit_dbtp"
    computed = analyzer.analyze_boundaries(
        derived.segments,
        limits,
        boundary_source=derived.source,
    )
    declared_by_index = {segment.index: segment for segment in document.segments}
    rows: list[dict[str, Any]] = []
    for analysis in computed:
        declared = declared_by_index.get(analysis.index)
        row: dict[str, Any] = {
            "index": analysis.index,
            "start_sample": analysis.start_sample,
            "end_sample": analysis.end_sample,
            "boundary_source": derived.source,
            "computed": {
                "short_term_lufs": round_db(analysis.short_term_lufs),
                "true_peak_dbtp": round_db(analysis.true_peak_dbtp),
                "true_peak_limit_dbtp": round_db(analysis.true_peak_limit_dbtp),
                "over_limit": analysis.over_limit,
                "excess_db": round_db(analysis.excess_db),
            },
            "declared": None,
            "effective": {
                "short_term_lufs": round_db(analysis.short_term_lufs),
                "true_peak_dbtp": round_db(analysis.true_peak_dbtp),
                "true_peak_limit_dbtp": round_db(analysis.true_peak_limit_dbtp),
                "over_limit": analysis.over_limit,
                "metric_source": "recomputed_from_wav",
                "limit_source": limit_source,
            },
            "declared_vs_computed": [],
        }
        if declared is not None:
            declared_values = {
                "short_term_lufs": round_db(declared.short_term_lufs),
                "true_peak_dbtp": round_db(declared.true_peak_dbtp),
                "true_peak_limit_dbtp": round_db(declared.true_peak_limit_dbtp),
                "over_limit": declared.over_limit,
                "excess_db": (
                    round_db(declared.excess_db)
                    if declared.excess_db is not None
                    else round_db(max(0.0, declared.true_peak_dbtp - declared.true_peak_limit_dbtp))
                ),
            }
            row["declared"] = declared_values
            row["effective"] = {
                **declared_values,
                "metric_source": "declared_metadata",
                "limit_source": (
                    "v3 segments[].true_peak_limit_dbtp"
                    if document.version == 3
                    else "v2 true_peak_limit_dbtp"
                ),
            }
            for field in (
                "short_term_lufs",
                "true_peak_dbtp",
                "true_peak_limit_dbtp",
                "over_limit",
            ):
                if not _values_equal(
                    field,
                    declared_values[field],
                    row["computed"][field],
                    tolerance,
                ):
                    row["declared_vs_computed"].append(
                        {
                            "field": field,
                            "declared": declared_values[field],
                            "recomputed": row["computed"][field],
                            "source": "declared_metadata",
                        }
                    )
        rows.append(row)

    weight_payload = [{"channel": name, "weight": weight} for name, weight in weights]
    report = {
        "version": document.version,
        "declared_integrated_lufs": round_db(document.integrated_lufs),
        "recomputed_integrated_lufs": round_db(analyzer.integrated_lufs()),
        "declared_target_offset_lu": round_db(document.target_offset_lu),
        "boundary_source": derived.source,
        "boundary_rule": derived.rule,
        "boundary_parameters": derived.parameters,
        "boundaries": [
            {"index": index, "start_sample": start, "end_sample": end}
            for index, (start, end) in enumerate(derived.segments)
        ],
        "downmix_weights": weight_payload,
        "downmix_weights_sha256": sha256_bytes(canonical_json_bytes(weight_payload)),
        "limit_source": limit_source,
        "reported_over_limit_set": (
            [segment.index for segment in document.segments if segment.over_limit]
            if document.segments
            else None
        ),
        "recomputed_over_limit_set": [
            analysis.index for analysis in computed if analysis.over_limit
        ],
        "segments": rows,
    }
    return report, computed, compare_boundaries(derived.segments, derived.segments)


def _compare_pair(
    left: dict[str, Any],
    right: dict[str, Any],
    tolerance: float,
) -> dict[str, Any]:
    left_rows = {row["index"]: row for row in left["segments"]}
    right_rows = {row["index"]: row for row in right["segments"]}
    indices = sorted(set(left_rows) | set(right_rows))
    diffs: list[dict[str, Any]] = []
    first: dict[str, Any] | None = None
    for index in indices:
        left_row = left_rows.get(index)
        right_row = right_rows.get(index)
        field_values: dict[str, Any] = {}
        mismatched_fields: list[str] = []
        reasons: list[dict[str, Any]] = []
        for field in _FIELD_ORDER:
            if field in {"start_sample", "end_sample"}:
                left_value = left_row.get(field) if left_row else None
                right_value = right_row.get(field) if right_row else None
                left_source = left["boundary_source"]
                right_source = right["boundary_source"]
            else:
                left_value = left_row["effective"].get(field) if left_row else None
                right_value = right_row["effective"].get(field) if right_row else None
                left_source = left_row["effective"]["metric_source"] if left_row else "missing"
                right_source = right_row["effective"]["metric_source"] if right_row else "missing"
            if left_row is None or right_row is None:
                equal = False
            else:
                equal = _values_equal(field, left_value, right_value, tolerance)
            field_values[field] = {
                "left": left_value,
                "right": right_value,
                "left_source": left_source,
                "right_source": right_source,
                "equal_within_tolerance": equal,
            }
            if not equal:
                mismatched_fields.append(field)
                reasons.append(
                    {
                        "field": field,
                        "left_source": left_source,
                        "right_source": right_source,
                    }
                )
        if (
            left_row is not None
            and right_row is not None
            and left_row["effective"]["over_limit"] != right_row["effective"]["over_limit"]
        ):
            reasons.append(
                {
                    "field": "over_limit",
                    "kind": "limit_or_metric_divergence",
                    "left_limit": left_row["effective"]["true_peak_limit_dbtp"],
                    "right_limit": right_row["effective"]["true_peak_limit_dbtp"],
                    "left_true_peak": left_row["effective"]["true_peak_dbtp"],
                    "right_true_peak": right_row["effective"]["true_peak_dbtp"],
                }
            )
        if mismatched_fields:
            diff = {
                "segment_index": index,
                "mismatched_fields": mismatched_fields,
                "fields": field_values,
                "reasons": reasons,
            }
            diffs.append(diff)
            if first is None:
                first = {
                    "segment_index": index,
                    "field": mismatched_fields[0],
                    "left_version": left["version"],
                    "right_version": right["version"],
                    "left_value": field_values[mismatched_fields[0]]["left"],
                    "right_value": field_values[mismatched_fields[0]]["right"],
                }
    boundary_diff = compare_boundaries(
        [
            (row["start_sample"], row["end_sample"])
            for row in left["segments"]
            if row["start_sample"] is not None
        ],
        [
            (row["start_sample"], row["end_sample"])
            for row in right["segments"]
            if row["start_sample"] is not None
        ],
    )
    return {
        "left_version": left["version"],
        "right_version": right["version"],
        "consistent": not diffs,
        "over_limit_sets": {
            "left_reported": left["reported_over_limit_set"],
            "right_reported": right["reported_over_limit_set"],
            "left_recomputed": left["recomputed_over_limit_set"],
            "right_recomputed": right["recomputed_over_limit_set"],
        },
        "boundary_comparison": boundary_diff.to_dict(),
        "segment_diffs": diffs,
        "first_mismatch": first,
    }


def review_versions(
    wav: WavInfo,
    documents: dict[int, MetadataDocument],
    config: Config,
) -> dict[str, Any]:
    normalized_documents: dict[int, MetadataDocument] = {}
    for key, document in documents.items():
        if isinstance(key, str):
            normalized_key = int(key.lower().lstrip("v"))
        else:
            normalized_key = int(key)
        if normalized_key != document.version:
            raise ValueError(
                f"document key {key!r} does not match document version {document.version}"
            )
        normalized_documents[normalized_key] = document
    documents = normalized_documents
    if not documents:
        raise ValueError("at least one metadata document is required")
    tolerance = (
        documents[3].quantization_tolerance_db
        if 3 in documents and documents[3].quantization_tolerance_db is not None
        else config.quantization_tolerance_db
    )
    reports: dict[str, Any] = {}
    computed_sets: dict[int, tuple[SegmentAnalysis, ...]] = {}
    for version in sorted(documents):
        report, computed, _ = _analysis_for(
            documents[version],
            wav,
            config,
            tolerance,
        )
        reports[f"v{version}"] = report
        computed_sets[version] = computed

    versions = sorted(documents)
    pair_order = sorted(
        itertools.combinations(versions, 2),
        key=lambda pair: (
            pair[1] != 3,
            pair[0] != 1,
            pair,
        ),
    )
    comparisons = [
        _compare_pair(reports[f"v{left}"], reports[f"v{right}"], tolerance)
        for left, right in pair_order
    ]
    mismatches = [
        (comparison["first_mismatch"], position)
        for position, comparison in enumerate(comparisons)
        if comparison["first_mismatch"] is not None
    ]
    first_mismatch = None
    if mismatches:
        first_mismatch = min(
            mismatches,
            key=lambda item: (
                item[0]["segment_index"],
                _FIELD_ORDER.index(item[0]["field"]),
                item[1],
            ),
        )[0]

    declared_mismatches: list[dict[str, Any]] = []
    for version in versions:
        report = reports[f"v{version}"]
        for row in report["segments"]:
            if row["declared_vs_computed"]:
                declared_mismatches.append(
                    {
                        "version": version,
                        "segment_index": row["index"],
                        "fields": row["declared_vs_computed"],
                    }
                )

    consistent = all(comparison["consistent"] for comparison in comparisons)
    wav_summary = wav.summary()
    wav_summary["path"] = Path(str(wav_summary["path"])).name
    result = {
        "schema": "loudness-review-report",
        "schema_version": 1,
        "ok": consistent,
        "code": None if consistent else E_SEGMENT_MISMATCH,
        "asset_id": next(iter(documents.values())).asset_id,
        "wav": wav_summary,
        "quantization_tolerance_db": tolerance,
        "versions_loaded": versions,
        "versions": reports,
        "comparisons": comparisons,
        "declared_vs_computed_mismatches": declared_mismatches,
        "first_mismatch": first_mismatch,
        "conclusion": (
            "all compared versions agree within the declared quantization tolerance"
            if consistent
            else "segment-level metadata disagrees; inspect first_mismatch and per-field sources"
        ),
        "reproduction": {
            "config_fingerprint": config.fingerprint,
            "analysis_algorithm_version": config.analysis_algorithm_version,
            "short_term_formula": (
                "LUFS=-0.691+10*log10(mean(sum_c(w_c*K_weighted(x_c)^2))) "
                "over the maximum short-term window in the segment"
            ),
            "true_peak_formula": (
                f"20*log10(max(abs(PCM samples and {config.true_peak_oversample}x "
                "Catmull-Rom interpolated samples across all channels)))"
            ),
            "over_limit_formula": "true_peak_dbtp > true_peak_limit_dbtp",
            "lufs_floor": LUFS_FLOOR,
            "true_peak_floor": TRUE_PEAK_FLOOR,
            "segment_data": {
                f"v{version}": [
                    {
                        key: row[key]
                        for key in (
                            "index",
                            "start_sample",
                            "end_sample",
                            "boundary_source",
                            "computed",
                            "declared",
                            "effective",
                        )
                    }
                    for row in reports[f"v{version}"]["segments"]
                ]
                for version in versions
            },
        },
    }
    return result

