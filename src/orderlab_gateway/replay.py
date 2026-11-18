from __future__ import annotations

import hashlib
import json
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from typing import Any

from .config import GatewayConfig
from .engine import QueryEngine, QueryRequest
from .errors import E_REQUEST, GatewayError


class ReplayRunner:
    def __init__(self, config: GatewayConfig, engine: QueryEngine) -> None:
        self.config = config
        self.engine = engine

    def load_samples(self) -> dict[str, Any]:
        return json.loads(self.config.samples_path.read_text(encoding="utf-8"))

    def run(
        self,
        *,
        concurrency: int | None = None,
        repeats: int = 1,
    ) -> dict[str, Any]:
        samples_doc = self.load_samples()
        samples = samples_doc.get("samples", [])
        if not samples:
            raise GatewayError(E_REQUEST, "sample set is empty")
        concurrency = concurrency or self.config.replay.concurrency
        if concurrency < 1 or concurrency > self.config.replay.max_concurrency:
            raise GatewayError(
                E_REQUEST,
                "replay concurrency is outside the configured CPU-safe range",
                details={
                    "received": concurrency,
                    "max_concurrency": self.config.replay.max_concurrency,
                },
            )
        if repeats < 1 or repeats > 10:
            raise GatewayError(E_REQUEST, "repeats must be between 1 and 10")

        jobs: list[tuple[dict[str, Any], str, int]] = []
        for repeat in range(repeats):
            for sample in samples:
                dialects = sample.get("dialects", {})
                for dialect in ("mssql-current", "mssql-legacy"):
                    if dialect not in dialects:
                        raise GatewayError(
                            E_REQUEST,
                            f"sample {sample.get('query_id')} is missing {dialect}",
                        )
                    jobs.append((sample, dialect, repeat))

        raw_results: list[dict[str, Any]] = []
        with ThreadPoolExecutor(max_workers=concurrency) as executor:
            futures = [
                executor.submit(self._run_one, sample, dialect, repeat)
                for sample, dialect, repeat in jobs
            ]
            for future in as_completed(futures):
                raw_results.append(future.result())

        records = sorted(
            raw_results,
            key=lambda record: (
                str(record["query_id"]),
                str(record["dialect"]),
                int(record["repeat"]),
            ),
        )
        aggregates = self._aggregate(records)
        samples_bytes = json.dumps(samples_doc, sort_keys=True).encode("utf-8")
        return {
            "schema_version": 1,
            "sample_set_sha256": hashlib.sha256(samples_bytes).hexdigest(),
            "concurrency": concurrency,
            "repeats": repeats,
            "queries_submitted": len(jobs),
            "categories": aggregates,
            "records": records,
        }

    def _run_one(
        self,
        sample: dict[str, Any],
        dialect: str,
        repeat: int,
    ) -> dict[str, Any]:
        query_id = str(sample["query_id"])
        category = str(sample["category"])
        fallback = str(sample.get("fallback", self.config.fallback.default))
        sql = str(sample["dialects"][dialect])
        try:
            response = self.engine.query(
                QueryRequest(
                    sql=sql,
                    dialect=dialect,
                    query_id=query_id,
                    fallback=fallback,
                )
            )
            if response["mode"] == "summary":
                outcome = "degraded"
            else:
                outcome = "succeeded"
            record = {
                "query_id": query_id,
                "dialect": dialect,
                "category": category,
                "repeat": repeat,
                "outcome": outcome,
                "submitted": 1,
                "succeeded": 1 if outcome == "succeeded" else 0,
                "rejected": 0,
                "degraded": 1 if outcome == "degraded" else 0,
                "error_code": None,
                "error_fragment": None,
                "index_name": ";".join(response.get("index_names", [])),
                "access": response.get("access"),
                "plan_rows": response.get("plan_rows", []),
                "row_count": response.get("row_count", 0),
                "columns": response.get("columns", []),
                "rows": response.get("rows", []),
                "result_digest": response.get("result_digest"),
                "summary_row_keys": response.get("summary_row_keys", []),
                "degradation_reason": (
                    response.get("degradation", {}).get("reason_code")
                    if outcome == "degraded"
                    else None
                ),
            }
        except GatewayError as exc:
            record = {
                "query_id": query_id,
                "dialect": dialect,
                "category": category,
                "repeat": repeat,
                "outcome": "rejected",
                "submitted": 1,
                "succeeded": 0,
                "rejected": 1,
                "degraded": 0,
                "error_code": exc.code,
                "error_fragment": exc.fragment,
                "index_name": "",
                "access": "not_executed",
                "plan_rows": exc.details.get("plan_rows", []),
                "row_count": 0,
                "columns": [],
                "rows": [],
                "result_digest": None,
                "summary_row_keys": [],
                "degradation_reason": None,
            }
        return record

    def _aggregate(self, records: list[dict[str, Any]]) -> list[dict[str, Any]]:
        categories = sorted({record["category"] for record in records})
        output = []
        for category in categories:
            subset = [record for record in records if record["category"] == category]
            output.append(
                {
                    "category": category,
                    "submitted": sum(record["submitted"] for record in subset),
                    "succeeded": sum(record["succeeded"] for record in subset),
                    "rejected": sum(record["rejected"] for record in subset),
                    "degraded": sum(record["degraded"] for record in subset),
                }
            )
        return output


def write_report(report: dict[str, Any], path: str | Path) -> None:
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(
        json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def compare_reports(
    baseline: dict[str, Any],
    disabled: dict[str, Any],
    restored: dict[str, Any],
) -> dict[str, Any]:
    baseline_map = _record_map(baseline)
    disabled_map = _record_map(disabled)
    restored_map = _record_map(restored)
    all_keys = sorted(set(baseline_map) | set(disabled_map) | set(restored_map))
    comparisons: list[dict[str, Any]] = []
    all_consistent = True
    for key in all_keys:
        before = baseline_map.get(key)
        during = disabled_map.get(key)
        after = restored_map.get(key)
        row_consistent = _same_result(before, after)
        disabled_is_distinct = _disabled_is_distinct(during, before)
        if not row_consistent:
            all_consistent = False
        comparisons.append(
            {
                "query_id": key[0],
                "dialect": key[1],
                "baseline_outcome": before.get("outcome") if before else None,
                "disabled_outcome": during.get("outcome") if during else None,
                "restored_outcome": after.get("outcome") if after else None,
                "baseline_error_code": before.get("error_code") if before else None,
                "disabled_error_code": during.get("error_code") if during else None,
                "restored_error_code": after.get("error_code") if after else None,
                "baseline_result_digest": before.get("result_digest") if before else None,
                "restored_result_digest": after.get("result_digest") if after else None,
                "row_consistent_after_restore": row_consistent,
                "disabled_reason_is_distinct": disabled_is_distinct,
            }
        )
    affected = sorted(
        {
            f"{record['query_id']}|{record['dialect']}"
            for record in disabled_map.values()
            if record.get("error_code") == "E_INDEX_UNAVAILABLE"
        }
    )
    return {
        "schema_version": 1,
        "all_rows_consistent_after_restore": all_consistent,
        "disabled_index_rejections": affected,
        "comparisons": comparisons,
    }


def _record_map(report: dict[str, Any]) -> dict[tuple[str, str], dict[str, Any]]:
    output: dict[tuple[str, str], dict[str, Any]] = {}
    for record in report.get("records", []):
        key = (str(record["query_id"]), str(record["dialect"]))
        previous = output.get(key)
        if previous is None or int(record.get("repeat", 0)) >= int(previous.get("repeat", 0)):
            output[key] = record
    return output


def _same_result(
    before: dict[str, Any] | None,
    after: dict[str, Any] | None,
) -> bool:
    if before is None or after is None:
        return False
    if before.get("outcome") != after.get("outcome"):
        return False
    if before.get("error_code") != after.get("error_code"):
        return False
    if before.get("outcome") == "rejected":
        return True
    return (
        before.get("columns") == after.get("columns")
        and before.get("rows") == after.get("rows")
        and before.get("result_digest") == after.get("result_digest")
    )


def _disabled_is_distinct(
    during: dict[str, Any] | None,
    baseline: dict[str, Any] | None,
) -> bool:
    if during is None or baseline is None:
        return False
    if baseline.get("outcome") != "succeeded":
        return True
    return during.get("error_code") == "E_INDEX_UNAVAILABLE"
