from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .compare import refresh_summary, validate_against_reference
from .config import (
    AggregateSpec,
    ConfigError,
    EngineConfig,
    PipelineConfig,
    RoundingSpec,
    SUPPORTED_OPS,
)
from .errors import BusinessIssue, ErrorCode
from .executor import run_aggregation
from .io_utils import dump_json_file
from .reference import compute_reference


def _comma_values(values: list[str] | None) -> list[str]:
    result: list[str] = []
    for value in values or []:
        result.extend(part.strip() for part in value.split(",") if part.strip())
    return result


def _unique_name(base: str, used: set[str]) -> str:
    candidate = base
    suffix = 2
    while candidate in used:
        candidate = f"{base}_{suffix}"
        suffix += 1
    used.add(candidate)
    return candidate


def _parse_aggregate_flag(text: str, used: set[str]) -> AggregateSpec:
    if "=" in text:
        name, expression = text.split("=", 1)
        name = name.strip()
    else:
        name = ""
        expression = text
    if ":" not in expression:
        raise ConfigError(
            f"聚合参数必须为 [name=]op:field，收到: {text}"
        )
    op_text, field_name = expression.split(":", 1)
    op = SUPPORTED_OPS.get(op_text.strip().lower())
    if op is None:
        raise ConfigError(f"不支持的聚合算子: {op_text}")
    field_name = field_name.strip()
    if not field_name:
        raise ConfigError(f"聚合字段不能为空: {text}")
    if not name:
        base = f"{op.lower()}_{field_name.replace('.', '_')}"
        name = _unique_name(base, used)
    elif name in used:
        raise ConfigError(f"聚合结果名称重复: {name}")
    else:
        used.add(name)
    return AggregateSpec(name=name, op=op, field=field_name)


def _parse_rounding_flags(values: list[str] | None) -> dict[str, dict[str, Any]]:
    result: dict[str, dict[str, Any]] = {}
    for value in values or []:
        if "=" not in value or "@" not in value:
            raise ConfigError(
                f"舍入参数必须为 target=mode@significant_digits，收到: {value}"
            )
        target, expression = value.split("=", 1)
        mode, digits_text = expression.split("@", 1)
        try:
            digits = int(digits_text)
        except ValueError as exc:
            raise ConfigError(f"有效位数不是整数: {digits_text}") from exc
        parsed = RoundingSpec.from_dict(
            {"mode": mode.strip(), "significant_digits": digits}
        )
        result[target.strip()] = {
            "mode": parsed.mode,
            "significant_digits": parsed.significant_digits,
        }
    return result


def _load_pipeline(args: argparse.Namespace) -> tuple[PipelineConfig, dict[str, Any]]:
    base_payload: dict[str, Any] = {}
    if args.pipeline:
        try:
            loaded = json.loads(Path(args.pipeline).read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise ConfigError(f"无法读取管道配置: {exc}") from exc
        if not isinstance(loaded, dict):
            raise ConfigError("管道配置文件必须是 JSON 对象")
        base_payload = loaded

    group_by = _comma_values(args.group_by)
    if not group_by:
        loaded_group_by = base_payload.get("group_by", [])
        if not isinstance(loaded_group_by, list):
            raise ConfigError("管道配置中的 group_by 必须是数组")
        group_by = [str(item) for item in loaded_group_by]

    loaded_aggregates = base_payload.get("aggregates", [])
    if not isinstance(loaded_aggregates, list):
        raise ConfigError("管道配置中的 aggregates 必须是数组")
    raw_aggregates = list(loaded_aggregates)
    if any(not isinstance(item, dict) for item in raw_aggregates):
        raise ConfigError("aggregates 中的每一项都必须是对象")
    used = {str(item.get("name")) for item in raw_aggregates if item.get("name")}
    for op_name in ("sum", "avg", "min", "max", "stddev"):
        for field_name in getattr(args, op_name) or []:
            base_name = f"{op_name}_{field_name.replace('.', '_')}"
            raw_aggregates.append(
                {
                    "name": _unique_name(base_name, used),
                    "op": op_name,
                    "field": field_name,
                }
            )
    for text in args.aggregate or []:
        raw_aggregates.append(_parse_aggregate_flag(text, used).__dict__)

    if not group_by:
        raise ConfigError("必须指定 --group-by 或在管道配置中提供 group_by")
    if not raw_aggregates:
        raise ConfigError("必须至少指定一个聚合算子")

    rounding_payload = dict(base_payload.get("rounding", {}))
    rounding_payload.update(_parse_rounding_flags(args.rounding))
    payload = {
        "group_by": group_by,
        "aggregates": raw_aggregates,
        "rounding": rounding_payload,
    }
    return PipelineConfig.from_dict(payload), base_payload


def _engine_config(
    args: argparse.Namespace,
    base_payload: dict[str, Any],
) -> EngineConfig:
    engine_payload = dict(base_payload.get("engine", {}))
    if args.batch_size is not None:
        engine_payload["batch_size"] = args.batch_size
    if args.max_active_groups is not None:
        engine_payload["max_active_groups"] = args.max_active_groups
    if args.max_state_bytes is not None:
        engine_payload["max_state_bytes"] = args.max_state_bytes
    if args.spill_partitions is not None:
        engine_payload["spill_partitions"] = args.spill_partitions
    if args.spill_dir is not None:
        engine_payload["spill_dir"] = args.spill_dir
    if args.fail_spill_on_attempt is not None:
        engine_payload["fail_spill_on_attempt"] = args.fail_spill_on_attempt
    if args.keep_spill:
        engine_payload["keep_spill"] = True
    return EngineConfig(**engine_payload)


def _write_report(
    payload: dict[str, Any],
    output: str | None,
    *,
    pretty: bool,
) -> None:
    if output in (None, "-"):
        if pretty:
            json.dump(
                payload,
                sys.stdout,
                ensure_ascii=False,
                sort_keys=True,
                indent=2,
                allow_nan=False,
            )
            sys.stdout.write("\n")
        else:
            json.dump(
                payload,
                sys.stdout,
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
                allow_nan=False,
            )
            sys.stdout.write("\n")
    else:
        dump_json_file(output, payload, pretty=pretty)


def _write_issue_stream(report: dict[str, Any], error_output: str | None) -> None:
    errors = report.get("errors", [])
    for error in errors:
        sys.stderr.write(
            json.dumps(
                error,
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
                allow_nan=False,
            )
            + "\n"
        )
    sys.stderr.flush()
    if error_output:
        dump_json_file(error_output, errors, pretty=True)


def _fatal(code: ErrorCode, reason: str, details: dict[str, Any] | None = None) -> int:
    issue = BusinessIssue(
        code=code,
        reason=reason,
        recoverable=False,
        details=details or {},
    )
    sys.stderr.write(
        json.dumps(
            issue.to_dict(),
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
            allow_nan=False,
        )
        + "\n"
    )
    return 4


def _exit_code(report: dict[str, Any], strict: bool) -> int:
    errors = report.get("errors", [])
    if any(error.get("code") == ErrorCode.PRECISION_BOUND.value for error in errors):
        return 2
    if strict and errors:
        return 3
    return 0


def _add_common_run_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--input", required=True, help="JSONL 样本路径")
    parser.add_argument("--pipeline", help="聚合管道 JSON 配置")
    parser.add_argument("--group-by", action="append", help="分组字段，可逗号分隔")
    parser.add_argument("--aggregate", action="append", help="[name=]op:field")
    for op_name in ("sum", "avg", "min", "max", "stddev"):
        parser.add_argument(
            f"--{op_name}",
            action="append",
            metavar="FIELD",
            help=f"对字段执行 {op_name}",
        )
    parser.add_argument("--rounding", action="append", help="target=mode@sig_digits")
    parser.add_argument("--batch-size", type=int)
    parser.add_argument("--max-active-groups", type=int)
    parser.add_argument("--max-state-bytes", type=int)
    parser.add_argument("--spill-partitions", type=int)
    parser.add_argument("--spill-dir")
    parser.add_argument("--fail-spill-on-attempt", type=int)
    parser.add_argument("--keep-spill", action="store_true")
    parser.add_argument("--output", default="-", help="结果 JSON 路径，- 表示 stdout")
    parser.add_argument("--error-output", help="错误数组 JSON 路径")
    parser.add_argument("--pretty", action="store_true")
    parser.add_argument("--strict", action="store_true", help="存在业务错误时退出非零")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="docagg",
        description="本地确定性文档聚合执行器",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    run_parser = subparsers.add_parser("run", help="执行聚合管道")
    _add_common_run_arguments(run_parser)
    run_parser.add_argument(
        "--reference",
        action="store_true",
        help="以固定样本计算参考真值并进行精度验收",
    )
    run_parser.add_argument(
        "--reference-file",
        help="使用已有参考真值 JSON 进行精度验收",
    )
    run_parser.add_argument(
        "--reference-output",
        help="将本次生成的参考真值写入 JSON",
    )

    ref_parser = subparsers.add_parser("reference", help="生成参考真值")
    _add_common_run_arguments(ref_parser)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        pipeline, base_payload = _load_pipeline(args)
        engine_config = _engine_config(args, base_payload)
    except (ConfigError, TypeError, ValueError) as exc:
        return _fatal(
            ErrorCode.INVALID_CONFIG,
            str(exc),
            getattr(exc, "details", {}),
        )

    try:
        if args.command == "reference":
            reference = compute_reference(
                args.input,
                pipeline,
                batch_size=engine_config.batch_size,
            )
            _write_report(reference, args.output, pretty=args.pretty)
            _write_issue_stream(reference, args.error_output)
            return _exit_code(reference, args.strict)

        report = run_aggregation(args.input, pipeline, engine_config)
        reference_payload: dict[str, Any] | None = None
        if args.reference:
            reference_payload = compute_reference(
                args.input,
                pipeline,
                batch_size=engine_config.batch_size,
            )
            if args.reference_output:
                dump_json_file(args.reference_output, reference_payload, pretty=args.pretty)
        elif args.reference_file:
            reference_payload = json.loads(
                Path(args.reference_file).read_text(encoding="utf-8")
            )
        if reference_payload is not None:
            validate_against_reference(report, reference_payload)
        else:
            refresh_summary(report)
        _write_report(report, args.output, pretty=args.pretty)
        _write_issue_stream(report, args.error_output)
        return _exit_code(report, args.strict)
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        return _fatal(
            ErrorCode.INPUT_INVALID,
            f"执行失败: {exc}",
        )


if __name__ == "__main__":
    raise SystemExit(main())
