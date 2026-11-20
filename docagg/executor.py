from __future__ import annotations

import json
import math
import os
import shutil
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

from .config import EngineConfig, PipelineConfig
from .errors import BusinessIssue, ErrorCode, NumericParseError
from .io_utils import ColumnarBatch, canonical_json, digest_payload
from .numeric import ParsedNumber, parse_numeric
from .rounding import apply_rounding
from .state import MetricState


def _key_id(key: dict[str, Any]) -> str:
    return json.dumps(
        key,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    )


@dataclass
class _GroupState:
    key: dict[str, Any]
    key_id: str
    documents: int = 0
    metrics: dict[str, MetricState] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "group_key": self.key,
            "group_key_id": self.key_id,
            "documents": self.documents,
            "metrics": {
                name: metric.to_dict() for name, metric in sorted(self.metrics.items())
            },
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> "_GroupState":
        key = dict(payload["group_key"])
        return cls(
            key=key,
            key_id=str(payload["group_key_id"]),
            documents=int(payload["documents"]),
            metrics={
                str(name): MetricState.from_dict(metric)
                for name, metric in payload.get("metrics", {}).items()
            },
        )


class AggregationEngine:
    def __init__(
        self,
        pipeline: PipelineConfig,
        config: EngineConfig | None = None,
        issue_callback: Callable[[BusinessIssue], None] | None = None,
    ) -> None:
        self.pipeline = pipeline
        self.config = config or EngineConfig()
        self.issue_callback = issue_callback
        self.groups: dict[str, _GroupState] = {}
        self.issues: list[BusinessIssue] = []
        self.invalid: set[tuple[str, str]] = set()
        self.documents = 0
        self.batches = 0
        self._finalized = False

        self._spill_root: Path | None = None
        self._spill_events: list[dict[str, Any]] = []
        self._spill_files: list[Path] = []
        self._successful_spills = 0
        self._failed_spills = 0
        self._spill_disabled = False
        self._spill_attempts = 0
        self._group_occurrences_before_merge = 0
        self._partial_group_ids: set[str] = set()
        self._distinct_groups_after_merge = 0
        self._merge_file_order: list[str] = []

    def _emit(self, issue: BusinessIssue) -> None:
        self.issues.append(issue)
        if self.issue_callback is not None:
            self.issue_callback(issue)

    def consume_batch(self, batch: ColumnarBatch) -> None:
        if self._finalized:
            raise RuntimeError("执行器已经完成，不能继续消费批次")
        self.batches += 1
        valid_rows: list[tuple[int, dict[str, Any], dict[str, Any], str]] = []

        for row_index, row in batch.iter_rows():
            self.documents += 1
            key: dict[str, Any] = {}
            invalid_key = False
            for field_name in self.pipeline.group_by:
                if field_name not in row or row[field_name] is None:
                    self._emit(
                        BusinessIssue(
                            ErrorCode.FIELD_MISSING,
                            "分组键字段缺失",
                            group_key=key or None,
                            field=field_name,
                            details={"row_index": row_index},
                        )
                    )
                    invalid_key = True
                    break
                value = row[field_name]
                if isinstance(value, (dict, list)) or (
                    isinstance(value, float) and not math.isfinite(value)
                ):
                    self._emit(
                        BusinessIssue(
                            ErrorCode.GROUP_KEY_INVALID,
                            "分组键必须是有限标量值",
                            group_key=key or None,
                            field=field_name,
                            details={"row_index": row_index},
                        )
                    )
                    invalid_key = True
                    break
                key[field_name] = value
            if invalid_key:
                continue
            try:
                key_id = _key_id(key)
            except (TypeError, ValueError) as exc:
                self._emit(
                    BusinessIssue(
                        ErrorCode.GROUP_KEY_INVALID,
                        "分组键无法序列化",
                        group_key=key or None,
                        details={"row_index": row_index, "error": str(exc)},
                    )
                )
                continue
            group = self.groups.get(key_id)
            if group is None:
                group = _GroupState(key=key, key_id=key_id)
                self.groups[key_id] = group
            group.documents += 1
            valid_rows.append((row_index, row, key, key_id))

        for aggregate in self.pipeline.aggregates:
            parsed_by_group: dict[str, list[ParsedNumber]] = {}
            for row_index, row, key, key_id in valid_rows:
                if (key_id, aggregate.name) in self.invalid:
                    continue
                if aggregate.field not in row or row[aggregate.field] is None:
                    self.invalid.add((key_id, aggregate.name))
                    self._emit(
                        BusinessIssue(
                            ErrorCode.FIELD_MISSING,
                            "数值字段缺失",
                            group_key=key,
                            field=aggregate.field,
                            aggregate=aggregate.name,
                            details={"row_index": row_index},
                        )
                    )
                    continue
                try:
                    number = parse_numeric(row[aggregate.field])
                except NumericParseError as exc:
                    self.invalid.add((key_id, aggregate.name))
                    self._emit(
                        BusinessIssue(
                            exc.code,
                            exc.reason,
                            group_key=key,
                            field=aggregate.field,
                            aggregate=aggregate.name,
                            details={"row_index": row_index, **exc.details},
                        )
                    )
                    continue
                parsed_by_group.setdefault(key_id, []).append(number)

            for key_id, values in parsed_by_group.items():
                if (key_id, aggregate.name) in self.invalid:
                    continue
                group = self.groups[key_id]
                existing_state = group.metrics.get(aggregate.name)
                combined_scales = set(
                    existing_state.decimal_scales if existing_state else set()
                )
                combined_scales.update(
                    number.scale
                    for number in values
                    if number.source_type == "decimal" and number.scale is not None
                )
                if len(combined_scales) > 1:
                    self.invalid.add((key_id, aggregate.name))
                    self._emit(
                        BusinessIssue(
                            ErrorCode.SCALE_MISMATCH,
                            "同一分组内定点小数的标度不一致",
                            group_key=group.key,
                            field=aggregate.field,
                            aggregate=aggregate.name,
                            details={
                                "scales": sorted(combined_scales),
                                "row_indices": [
                                    row_index
                                    for row_index, _, _, candidate_key_id in valid_rows
                                    if candidate_key_id == key_id
                                ],
                            },
                        )
                    )
                    continue
                metric = existing_state
                if metric is None:
                    metric = MetricState(
                        name=aggregate.name,
                        op=aggregate.op,
                        field=aggregate.field,
                    )
                    group.metrics[aggregate.name] = metric
                metric.update_many(values)

        self._maybe_spill()

    def _state_bytes(self) -> int:
        total = 0
        for group in self.groups.values():
            total += len(canonical_json(group.to_dict()).encode("utf-8"))
        return total

    def _maybe_spill(self) -> None:
        if self._spill_disabled or not self.groups:
            return
        groups_exceeded = (
            self.config.max_active_groups is not None
            and len(self.groups) > self.config.max_active_groups
        )
        bytes_exceeded = (
            self.config.max_state_bytes is not None
            and self._state_bytes() > self.config.max_state_bytes
        )
        if groups_exceeded or bytes_exceeded:
            self._spill()

    def _ensure_spill_root(self) -> Path:
        if self._spill_root is not None:
            return self._spill_root
        if self.config.spill_dir:
            base = Path(self.config.spill_dir)
            base.mkdir(parents=True, exist_ok=True)
            self._spill_root = Path(
                tempfile.mkdtemp(prefix="docagg-spill-", dir=str(base))
            )
        else:
            self._spill_root = Path(tempfile.mkdtemp(prefix="docagg-spill-"))
        return self._spill_root

    def _spill(self) -> bool:
        if not self.groups or self._spill_disabled:
            return False
        self._spill_attempts += 1
        attempt = self._spill_attempts
        if attempt < 1:
            raise RuntimeError("invalid spill attempt counter")

        if self.config.fail_spill_on_attempt == attempt:
            self._failed_spills += 1
            self._spill_disabled = True
            self._emit(
                BusinessIssue(
                    ErrorCode.SPILL_WRITE_FAILED,
                    "溢出分区文件写入失败（已注入故障），保留内存状态继续执行",
                    recoverable=True,
                    details={"attempt": attempt},
                )
            )
            return False

        spill_index = len(self._spill_events) + 1
        try:
            root = self._ensure_spill_root()
        except OSError as exc:
            self._failed_spills += 1
            self._spill_disabled = True
            self._emit(
                BusinessIssue(
                    ErrorCode.SPILL_WRITE_FAILED,
                    "无法创建溢出分区目录，保留内存状态继续执行",
                    recoverable=True,
                    details={"attempt": attempt, "error": str(exc)},
                )
            )
            return False
        buckets: list[list[_GroupState]] = [
            [] for _ in range(self.config.spill_partitions)
        ]
        for position, key_id in enumerate(sorted(self.groups)):
            buckets[position % self.config.spill_partitions].append(
                self.groups[key_id]
            )

        pending: list[tuple[Path, Path]] = []
        final_paths: list[Path] = []
        try:
            for partition_index, bucket in enumerate(buckets):
                if not bucket:
                    continue
                name = (
                    f"spill-{spill_index:06d}"
                    f"-part-{partition_index:03d}.jsonl"
                )
                final_path = root / name
                temp_path = root / (name + ".tmp")
                with temp_path.open("w", encoding="utf-8") as handle:
                    for group in bucket:
                        handle.write(canonical_json(group.to_dict()))
                        handle.write("\n")
                    handle.flush()
                    os.fsync(handle.fileno())
                pending.append((temp_path, final_path))

            for temp_path, final_path in pending:
                os.replace(temp_path, final_path)
                final_paths.append(final_path)
        except OSError as exc:
            for temp_path, _ in pending:
                try:
                    temp_path.unlink()
                except FileNotFoundError:
                    pass
            for final_path in final_paths:
                try:
                    final_path.unlink()
                except FileNotFoundError:
                    pass
            self._failed_spills += 1
            self._spill_disabled = True
            self._emit(
                BusinessIssue(
                    ErrorCode.SPILL_WRITE_FAILED,
                    "溢出分区文件写入失败，保留内存状态继续执行",
                    recoverable=True,
                    details={"attempt": attempt, "error": str(exc)},
                )
            )
            return False

        state_bytes = sum(
            len(canonical_json(group.to_dict()).encode("utf-8"))
            for bucket in buckets
            for group in bucket
        )
        event = {
            "spill_index": spill_index,
            "partition_count": self.config.spill_partitions,
            "nonempty_partitions": len(final_paths),
            "files": [path.name for path in final_paths],
            "group_occurrences": len(self.groups),
            "state_bytes": state_bytes,
        }
        self._spill_events.append(event)
        self._spill_files.extend(final_paths)
        self._successful_spills += 1
        self.groups = {}
        return True

    def _merge_group(self, incoming: _GroupState) -> None:
        existing = self.groups.get(incoming.key_id)
        if existing is None:
            self.groups[incoming.key_id] = incoming
            return
        existing.documents += incoming.documents
        for name, incoming_metric in incoming.metrics.items():
            existing_metric = existing.metrics.get(name)
            if existing_metric is None:
                existing.metrics[name] = incoming_metric
            else:
                combined_scales = (
                    set(existing_metric.decimal_scales)
                    | set(incoming_metric.decimal_scales)
                )
                if (
                    len(combined_scales) > 1
                    and (incoming.key_id, name) not in self.invalid
                ):
                    self.invalid.add((incoming.key_id, name))
                    self._emit(
                        BusinessIssue(
                            ErrorCode.SCALE_MISMATCH,
                            "跨溢出分区合并时发现定点小数标度不一致",
                            group_key=existing.key,
                            field=incoming_metric.field,
                            aggregate=name,
                            details={"scales": sorted(combined_scales)},
                        )
                    )
                existing_metric.merge(incoming_metric)

    def _merge_spilled_partials(self) -> None:
        if not self._spill_files:
            self._distinct_groups_after_merge = len(self.groups)
            return
        in_memory = self.groups
        self.groups = {}
        for path in sorted(self._spill_files, key=lambda item: item.name):
            self._merge_file_order.append(path.name)
            try:
                with path.open("r", encoding="utf-8") as handle:
                    for line in handle:
                        text = line.strip()
                        if not text:
                            continue
                        payload = json.loads(text)
                        incoming = _GroupState.from_dict(payload)
                        self._group_occurrences_before_merge += 1
                        self._partial_group_ids.add(incoming.key_id)
                        self._merge_group(incoming)
            except (OSError, ValueError, KeyError, TypeError) as exc:
                self._emit(
                    BusinessIssue(
                        ErrorCode.PARTIAL_STATE_INVALID,
                        "无法读取或合并溢出分区部分聚合",
                        recoverable=True,
                        details={"file": path.name, "error": str(exc)},
                    )
                )
        for incoming in sorted(in_memory.values(), key=lambda item: item.key_id):
            self._group_occurrences_before_merge += 1
            self._partial_group_ids.add(incoming.key_id)
            self._merge_group(incoming)
        self._distinct_groups_after_merge = len(self.groups)

    def _spill_report(self) -> dict[str, Any]:
        merge_consistent = (
            True
            if self._successful_spills == 0
            else len(self._partial_group_ids) == self._distinct_groups_after_merge
        )
        payload: dict[str, Any] = {
            "triggered": bool(self._spill_events) or self._failed_spills > 0,
            "successful_spills": self._successful_spills,
            "failed_spills": self._failed_spills,
            "spill_attempts": self._spill_attempts,
            "events": self._spill_events,
            "group_occurrences_before_merge": self._group_occurrences_before_merge,
            "distinct_groups_in_partials": len(self._partial_group_ids),
            "distinct_groups_after_merge": self._distinct_groups_after_merge,
            "group_count_consistent": merge_consistent,
            "merge_order": self._merge_file_order,
            "error_bound_policy": (
                "partial bounds are added and each merge adds a binary64 "
                "rounding allowance; the declared result bound is not reduced "
                "after spill or merge"
            ),
            "spill_root_retained": (
                str(self._spill_root)
                if self.config.keep_spill and self._spill_root is not None
                else None
            ),
        }
        return payload

    def _issues_for(self, group_key: dict[str, Any], aggregate_name: str) -> list[BusinessIssue]:
        return [
            issue
            for issue in self.issues
            if issue.group_key == group_key and issue.aggregate == aggregate_name
        ]

    def finalize(self) -> dict[str, Any]:
        if self._finalized:
            raise RuntimeError("执行器已经完成")
        self._finalized = True
        if self._successful_spills:
            if self.groups:
                self._spill()
            self._merge_spilled_partials()
        else:
            self._distinct_groups_after_merge = len(self.groups)

        groups_payload: list[dict[str, Any]] = []
        for key_id in sorted(self.groups):
            group = self.groups[key_id]
            aggregate_payload: dict[str, Any] = {}
            for aggregate in self.pipeline.aggregates:
                metric = group.metrics.get(aggregate.name)
                sample_count = metric.count if metric is not None else 0
                if aggregate.op == "stdDev" and sample_count < 2:
                    existing_stddev_issue = any(
                        issue.code == ErrorCode.STDDEV_INSUFFICIENT_SAMPLES
                        and issue.group_key == group.key
                        and issue.aggregate == aggregate.name
                        for issue in self.issues
                    )
                    if not existing_stddev_issue:
                        self._emit(
                            BusinessIssue(
                                ErrorCode.STDDEV_INSUFFICIENT_SAMPLES,
                                "stdDev 至少需要两个有效样本",
                                group_key=group.key,
                                field=aggregate.field,
                                aggregate=aggregate.name,
                                details={"sample_count": sample_count},
                            )
                        )
                issues = self._issues_for(group.key, aggregate.name)
                if (key_id, aggregate.name) in self.invalid or issues:
                    codes = sorted({issue.code.value for issue in issues})
                    aggregate_payload[aggregate.name] = {
                        "status": "error",
                        "name": aggregate.name,
                        "op": aggregate.op,
                        "field": aggregate.field,
                        "error_codes": codes,
                    }
                    continue
                if metric is None:
                    aggregate_payload[aggregate.name] = {
                        "status": "error",
                        "name": aggregate.name,
                        "op": aggregate.op,
                        "field": aggregate.field,
                        "error_codes": [ErrorCode.FIELD_MISSING.value],
                    }
                    continue
                result = metric.result()
                if result is None:
                    self._emit(
                        BusinessIssue(
                            ErrorCode.NUMERIC_OVERFLOW,
                            "聚合结果超出双精度范围或无法形成有限值",
                            group_key=group.key,
                            field=aggregate.field,
                            aggregate=aggregate.name,
                        )
                    )
                    aggregate_payload[aggregate.name] = {
                        "status": "error",
                        "name": aggregate.name,
                        "op": aggregate.op,
                        "field": aggregate.field,
                        "error_codes": [ErrorCode.NUMERIC_OVERFLOW.value],
                    }
                    continue

                audit_value = result.pop("_audit_decimal")
                result["audit_value_text"] = metric.audit_text(audit_value)
                result["status"] = "ok"
                result["name"] = aggregate.name
                result["op"] = aggregate.op
                result["field"] = aggregate.field
                rounding_result = apply_rounding(
                    result["value"],
                    self.pipeline.rounding_for(aggregate),
                )
                result["rounding"] = rounding_result
                result["value_before_round"] = rounding_result[
                    "value_before_round"
                ]
                result["rounded_value"] = rounding_result["rounded_value"]
                result["rounded_value_text"] = rounding_result[
                    "rounded_value_text"
                ]
                result["post_round_error_estimate"] = (
                    result["error_estimate"]
                    + rounding_result["rounding_uncertainty"]
                )
                if not math.isfinite(result["post_round_error_estimate"]):
                    result["post_round_error_estimate"] = float.fromhex(
                        "0x1.fffffffffffffp+1023"
                    )
                result["error_estimate_basis"] = "value_before_round"
                result["type_downgraded"] = result["precision"][
                    "type_downgraded"
                ]
                result["source_types"] = result["precision"]["source_types"]
                result["decimal_scales"] = result["precision"][
                    "decimal_scales"
                ]
                result["decimal_precision_digits"] = result["precision"][
                    "decimal_precision_digits"
                ]
                aggregate_payload[aggregate.name] = result
            groups_payload.append(
                {
                    "key": group.key,
                    "document_count": group.documents,
                    "aggregates": aggregate_payload,
                }
            )

        payload: dict[str, Any] = {
            "schema_version": "1.0",
            "pipeline": self.pipeline.to_dict(),
            "engine_config": {
                "batch_size": self.config.batch_size,
                "max_active_groups": self.config.max_active_groups,
                "max_state_bytes": self.config.max_state_bytes,
                "spill_partitions": self.config.spill_partitions,
            },
            "documents": self.documents,
            "batches": self.batches,
            "groups": groups_payload,
            "errors": [issue.to_dict() for issue in self.issues],
            "spill": self._spill_report(),
        }
        payload["summary"] = {
            "group_count": len(groups_payload),
            "error_count": len(self.issues),
            "precision_error_count": sum(
                issue.code == ErrorCode.PRECISION_BOUND for issue in self.issues
            ),
            "deterministic_digest": digest_payload(
                {"groups": payload["groups"], "errors": payload["errors"]}
            ),
        }

        if self._spill_root is not None and not self.config.keep_spill:
            shutil.rmtree(self._spill_root, ignore_errors=True)
            self._spill_root = None
        return payload


def run_aggregation(
    path: str | os.PathLike[str],
    pipeline: PipelineConfig,
    engine_config: EngineConfig | None = None,
) -> dict[str, Any]:
    from .io_utils import count_jsonl_records, iter_jsonl_batches, sha256_file

    config = engine_config or EngineConfig()
    engine = AggregationEngine(pipeline, config)
    for batch in iter_jsonl_batches(path, config.batch_size):
        engine.consume_batch(batch)
    payload = engine.finalize()
    payload["input"] = {
        "path": str(path),
        "sha256": sha256_file(path),
        "documents": count_jsonl_records(path),
    }
    return payload
