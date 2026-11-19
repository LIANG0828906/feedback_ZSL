"""Deterministic JSON report assembly."""
from __future__ import annotations

import hashlib
import json

from .clients import PROTOS
from .config import Config
from .dataset import Dataset

SCHEMA_VERSION = 1


def _iter_records(obj):
    """Yield every request record (dicts carrying an 'age' key)."""
    if isinstance(obj, dict):
        if "age" in obj and "cache_hit" in obj:
            yield obj
        for value in obj.values():
            yield from _iter_records(value)
    elif isinstance(obj, list):
        for item in obj:
            yield from _iter_records(item)


def build_report(config: Config, dataset: Dataset, *,
                 topology: dict, varnish: dict, cache_key: dict,
                 main: dict, vary: list, head: list, revalidate: list,
                 chunked: list, upstream: list, errors: dict) -> dict:
    urls = []
    vary_by_url = {v["url"]: v for v in vary}
    head_by_url = {h["url"]: h for h in head}
    reval_by_url = {r["url"]: r for r in revalidate}
    chunked_by_url = {c["url"]: c for c in chunked}

    for sample in dataset.samples:
        slot = main[sample.path]
        main_block = {p: slot[p] for p in PROTOS}
        all_recs = [rec for p in PROTOS for rec in slot[p]]
        expected_sha = hashlib.sha256(
            dataset.identity_body(sample)).hexdigest()
        entry = {
            "backend": sample.backend,
            "body_equivalent": len({r["body_sha256"] for r in all_recs}) == 1
            and all(r["status"] == 200 for r in all_recs),
            "expected": {
                "body_length": sample.size,
                "body_sha256": expected_sha,
                "encoding_bucket": "identity",
            },
            "flags": {
                "chunked": sample.chunked,
                "etag": sample.etag,
                "head_mismatch": sample.head_mismatch,
                "vary": sample.vary,
            },
            "main": main_block,
            "matches_expected": all(
                r["status"] == 200 and r["body_sha256"] == expected_sha
                and r["body_length"] == sample.size for r in all_recs),
            "path": sample.path,
            "sample_id": sample.sample_id,
            "shared_cache_hit": main_block[PROTOS[1]][0]["cache_hit"],
        }
        if sample.path in vary_by_url:
            entry["vary"] = vary_by_url[sample.path]
        if sample.path in head_by_url:
            entry["head"] = head_by_url[sample.path]
        if sample.path in reval_by_url:
            entry["revalidate"] = reval_by_url[sample.path]
        if sample.path in chunked_by_url:
            entry["chunked"] = chunked_by_url[sample.path]
        if sample.backend == "C":
            upstream_protos = slot.get("_upstream_proto", set())
            entry["upstream_upgrade_ok"] = upstream_protos == {"h2"}
        urls.append(entry)

    hit_counts = {p: sum(1 for u in urls for r in u["main"][p]
                         if r["cache_hit"]) for p in PROTOS}
    total_requests = len(urls) * 2
    expected_errors = {
        "E_HEAD_MISMATCH": 10,
        "E_REVALIDATE": 0,
        "E_UPSTREAM_PROTOCOL": 2,
    }
    unexpected = {code: max(0, errors.get(code, 0) - expected_errors[code])
                  for code in expected_errors}

    age_ok = True
    for container in (urls, revalidate, vary):
        for rec in _iter_records(container):
            if not rec["age"]["header_present"]:
                age_ok = False
            want = "fresh_hit" if rec["cache_hit"] else "origin"
            if rec["age"]["semantics"] != want:
                age_ok = False

    summary = {
        "age_semantics_consistent": age_ok,
        "body_equivalence_urls": sum(1 for u in urls if u["body_equivalent"]),
        "cache_hit_counts": hit_counts,
        "chunked_integrity": sum(1 for c in chunked if c["ok"]),
        "cross_protocol_shared_hits": sum(1 for u in urls
                                          if u["shared_cache_hit"]),
        "expected_error_counts": expected_errors,
        "head_mismatch_detected": sum(
            1 for h in head if h["error_code"] == "E_HEAD_MISMATCH"),
        "head_no_pollution": sum(1 for h in head
                                 if h["head_does_not_pollute_get"]),
        "main_expectations_ok": all(u["matches_expected"] for u in urls),
        "protocol_hit_rate": {
            p: {
                "hits": hit_counts[p],
                "rate": hit_counts[p] / total_requests,
                "requests": total_requests,
            } for p in PROTOS
        },
        "revalidate_304": sum(
            1 for r in revalidate
            if all(r[p]["conditional_ok"] for p in PROTOS)),
        "revalidate_cache_fills": sum(
            1 for r in revalidate
            if any(r[p]["counts_as_cache_fill"] for p in PROTOS)),
        "unexpected_error_counts": unexpected,
        "upstream_forced_uncached": sum(
            1 for u in upstream for e in u["events"] if not e["cache_hit"]),
        "vary_buckets_consistent": sum(1 for v in vary if v["ok"]),
    }

    ok = (
        all(v == 0 for v in unexpected.values())
        and errors.get("E_HEAD_MISMATCH") == expected_errors["E_HEAD_MISMATCH"]
        and errors.get("E_UPSTREAM_PROTOCOL")
        == expected_errors["E_UPSTREAM_PROTOCOL"]
        and summary["main_expectations_ok"]
        and summary["body_equivalence_urls"] == len(urls)
        and summary["cross_protocol_shared_hits"] == len(urls)
        and summary["vary_buckets_consistent"] == len(vary)
        and summary["head_no_pollution"] == len(head)
        and summary["chunked_integrity"] == len(chunked)
        and summary["revalidate_304"] == len(revalidate)
        and summary["revalidate_cache_fills"] == 0
        and summary["upstream_forced_uncached"] == 2
        and summary["age_semantics_consistent"]
        and all(u.get("upstream_upgrade_ok", True) for u in urls)
    )

    return {
        "cache_key": cache_key,
        "chunked_checks": chunked,
        "config": {
            "backend_bind": config.bind,
            "ports": config.all_ports(),
            "seed": config.seed,
            "url_count": len(urls),
            "varnish_backend_host": config.bind,
            "varnish_mode": config.varnish_mode,
        },
        "dataset": {
            "chunked": len(dataset.select(chunked=True)),
            "etag": len(dataset.select(etag=True)),
            "head_mismatch": len(dataset.select(head_mismatch=True)),
            "total_bytes": dataset.total_bytes(),
            "urls": len(dataset.samples),
            "vary": len(dataset.select(vary=True)),
        },
        "error_counts": errors,
        "head_checks": head,
        "ok": ok,
        "revalidate_checks": revalidate,
        "schema_version": SCHEMA_VERSION,
        "summary": summary,
        "topology": topology,
        "upstream_protocol_checks": upstream,
        "urls": urls,
        "varnish": varnish,
        "vary_checks": vary,
    }


def cache_key_section() -> dict:
    return {
        "hash_inputs": [
            "req.http.host",
            "req.url",
            "canonical Accept-Encoding for /a/ Vary URLs",
        ],
        "includes_client_protocol": False,
        "strategy_id": "host-url-vary-v1",
        "vary_buckets": ["identity", "gzip"],
    }


def dump_report(report: dict) -> str:
    return json.dumps(report, indent=2, sort_keys=True) + "\n"


def topology_failure_report(config: Config, detail: str,
                            protocols: dict | None = None) -> dict:
    return {
        "config": {
            "backend_bind": config.bind,
            "ports": config.all_ports(),
            "seed": config.seed,
            "url_count": config.url_count,
            "varnish_backend_host": config.bind,
            "varnish_mode": config.varnish_mode,
        },
        "error": {"code": "E_TOPOLOGY", "detail": detail},
        "error_counts": {"E_TOPOLOGY": 1},
        "ok": False,
        "schema_version": SCHEMA_VERSION,
        "topology": {
            "management_reachable": False,
            "protocols": protocols or {},
        },
    }
