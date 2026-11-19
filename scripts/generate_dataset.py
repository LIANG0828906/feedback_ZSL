from __future__ import annotations

import argparse
import json
import random
import sys
import time
from dataclasses import dataclass
from datetime import timedelta
from http.client import HTTPConnection
from pathlib import Path
from urllib.parse import quote, urlsplit

from archive_service.util import format_time, parse_time, utc_now


SMALL_MIN = 4 * 1024
SMALL_MAX = 200 * 1024
MEDIUM_MIN = 1 * 1024 * 1024
MEDIUM_MAX = 5 * 1024 * 1024
MIB = 1024 * 1024


@dataclass(frozen=True)
class VersionSpec:
    object_key: str
    version_number: int
    category: str
    size: int
    written_at: str


class ApiClient:
    def __init__(self, base_url: str, timeout: float = 120.0):
        parsed = urlsplit(base_url)
        if parsed.scheme != "http" or not parsed.hostname:
            raise ValueError("base URL must be http://host:port")
        self.host = parsed.hostname
        self.port = parsed.port or 80
        self.timeout = timeout

    def request(
        self,
        method: str,
        path: str,
        *,
        body: bytes | None = None,
        headers: dict[str, str] | None = None,
    ) -> tuple[int, bytes]:
        connection = HTTPConnection(self.host, self.port, timeout=self.timeout)
        try:
            connection.request(method, path, body=body, headers=headers or {})
            response = connection.getresponse()
            return response.status, response.read()
        finally:
            connection.close()

    def request_json(
        self,
        method: str,
        path: str,
        payload: dict,
    ) -> tuple[int, dict]:
        status, raw = self.request(
            method,
            path,
            body=json.dumps(payload, separators=(",", ":")).encode("utf-8"),
            headers={"Content-Type": "application/json"},
        )
        try:
            result = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            result = {"raw": raw[:500].decode("utf-8", "replace")}
        return status, result


def _version_count(index: int, *, kind: str) -> int:
    if kind == "small":
        return 2 if index % 6 == 0 else 1
    return 2 if index % 3 == 0 else 1


def _small_category(index: int) -> str:
    return ("regular", "audit", "legal")[index % 3]


def build_specs(
    *,
    seed: int,
    small_objects: int,
    medium_objects: int,
    target_mib: int,
    written_days_ago: int,
    reference_time: str | None,
) -> tuple[list[VersionSpec], dict]:
    sizes_rng = random.Random(seed)
    reference = parse_time(reference_time, default=utc_now())
    target_bytes = target_mib * MIB
    small_specs: list[tuple[str, int, str, int]] = []
    for index in range(small_objects):
        key = f"records/small/{index // 100:02d}/obj-{index:04d}.bin"
        category = _small_category(index)
        for version in range(1, _version_count(index, kind="small") + 1):
            size = sizes_rng.randint(SMALL_MIN, SMALL_MAX)
            small_specs.append((key, version, category, size))

    medium_versions = sum(
        _version_count(index, kind="medium") for index in range(medium_objects)
    )
    small_total = sum(item[3] for item in small_specs)
    medium_budget = max(medium_versions * MEDIUM_MIN, target_bytes - small_total)
    medium_sizes: list[int] = []
    remaining = medium_budget
    for index in range(medium_versions):
        left = medium_versions - index
        min_remaining = max(0, left - 1) * MEDIUM_MIN
        max_remaining = max(0, left - 1) * MEDIUM_MAX
        ideal = remaining / left
        weighted = max(MEDIUM_MIN, min(MEDIUM_MAX, int(ideal * sizes_rng.uniform(0.88, 1.12))))
        low = max(MEDIUM_MIN, remaining - max_remaining)
        high = min(MEDIUM_MAX, remaining - min_remaining)
        size = max(low, min(high, weighted))
        medium_sizes.append(size)
        remaining -= size

    medium_specs: list[tuple[str, int, str, int]] = []
    cursor = 0
    for index in range(medium_objects):
        key = f"records/audit-reports/{index // 100:02d}/report-{index:04d}.bin"
        for version in range(1, _version_count(index, kind="medium") + 1):
            medium_specs.append(
                (key, version, "audit", medium_sizes[cursor])
            )
            cursor += 1

    specs: list[VersionSpec] = []
    for key, version, category, size in small_specs + medium_specs:
        jitter_seconds = sizes_rng.randint(0, 23 * 60 * 60)
        written = reference - timedelta(
            days=written_days_ago, seconds=jitter_seconds
        )
        specs.append(
            VersionSpec(
                object_key=key,
                version_number=version,
                category=category,
                size=size,
                written_at=format_time(written),
            )
        )
    specs.sort(key=lambda item: (item.object_key, item.version_number))
    summary = {
        "small_object_count": small_objects,
        "medium_object_count": medium_objects,
        "small_version_count": len(small_specs),
        "medium_version_count": len(medium_specs),
        "total_version_count": len(specs),
        "original_bytes": sum(item.size for item in specs),
        "three_domain_bytes": sum(item.size for item in specs) * 3,
        "target_bytes": target_bytes,
    }
    return specs, summary


def _content_for(seed: int, spec: VersionSpec) -> bytes:
    generator = random.Random(f"{seed}:{spec.object_key}:{spec.version_number}")
    return generator.randbytes(spec.size)


def generate(args: argparse.Namespace) -> dict:
    specs, summary = build_specs(
        seed=args.seed,
        small_objects=args.small_objects,
        medium_objects=args.medium_objects,
        target_mib=args.target_mib,
        written_days_ago=args.written_days_ago,
        reference_time=args.reference_time,
    )
    if args.dry_run:
        summary["dry_run"] = True
        summary["sample_objects"] = [
            {
                "category": spec.category,
                "object_key": spec.object_key,
                "size": spec.size,
                "version_number": spec.version_number,
                "written_at": spec.written_at,
            }
            for spec in specs[:5]
        ]
        return summary

    client = ApiClient(args.base_url, timeout=args.timeout)
    health_status, health_body = client.request("GET", "/healthz")
    if health_status != 200:
        raise RuntimeError(f"service health check failed: {health_status} {health_body!r}")

    written_versions: list[dict] = []
    started = time.monotonic()
    for index, spec in enumerate(specs, start=1):
        content = _content_for(args.seed, spec)
        path = (
            "/api/v1/objects?key="
            + quote(spec.object_key, safe="")
            + f"&category={spec.category}"
        )
        status, raw = client.request(
            "PUT",
            path,
            body=content,
            headers={
                "Content-Type": "application/octet-stream",
                "X-Archive-Written-At": spec.written_at,
            },
        )
        if status != 201:
            raise RuntimeError(
                f"write failed for {spec.object_key} v{spec.version_number}: "
                f"{status} {raw[:500]!r}"
            )
        response = json.loads(raw.decode("utf-8"))
        written_versions.append(response)
        if not args.quiet and (index == 1 or index % 20 == 0 or index == len(specs)):
            elapsed = time.monotonic() - started
            print(
                f"[{index}/{len(specs)}] {response['content_length']} bytes, "
                f"{elapsed:.1f}s",
                file=sys.stderr,
                flush=True,
            )

    policy = None
    if args.extend_policy:
        status, result = client.request_json(
            "POST",
            "/api/v1/policies",
            {
                "regular_days": args.policy_days,
                "audit_days": args.policy_days,
                "legal_days": args.policy_days,
            },
        )
        if status != 201:
            raise RuntimeError(f"policy publication failed: {status} {result}")
        policy = result

    conflicts = []
    if args.conflicts:
        candidates = [
            item for item in written_versions if item["category"] == "audit"
        ][: args.conflicts]
        if len(candidates) < args.conflicts:
            raise RuntimeError(
                f"requested {args.conflicts} conflicts but only "
                f"{len(candidates)} audit versions exist"
            )
        for item in candidates:
            written = parse_time(item["written_at"])
            metadata_until = format_time(written + timedelta(days=args.policy_days))
            lock_until = format_time(written + timedelta(days=args.conflict_lock_days))
            status, result = client.request_json(
                "PUT",
                f"/api/v1/admin/versions/{item['version_id']}/retention",
                {
                    "lock_pinned": True,
                    "lock_retention_until": lock_until,
                    "metadata_retention_until": metadata_until,
                },
            )
            if status != 200:
                raise RuntimeError(
                    f"conflict injection failed for {item['version_id']}: "
                    f"{status} {result}"
                )
            conflicts.append(
                {
                    "lock_retention_until": lock_until,
                    "metadata_retention_until": metadata_until,
                    "object_key": item["object_key"],
                    "version_id": item["version_id"],
                }
            )

    summary.update(
        {
            "base_url": args.base_url,
            "conflict_count": len(conflicts),
            "conflicts": conflicts,
            "elapsed_seconds": round(time.monotonic() - started, 3),
            "policy_publication": policy,
            "sample_versions": written_versions[:3],
            "seed": args.seed,
            "written_days_ago": args.written_days_ago,
        }
    )
    if args.output:
        Path(args.output).write_text(
            json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
    return summary


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Generate a deterministic three-domain archive sample"
    )
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--seed", type=int, default=283)
    parser.add_argument("--small-objects", type=int, default=120)
    parser.add_argument("--medium-objects", type=int, default=30)
    parser.add_argument("--target-mib", type=int, default=150)
    parser.add_argument("--written-days-ago", type=int, default=210)
    parser.add_argument(
        "--reference-time",
        default=None,
        help="fixed ISO-8601 base time for reproducible written_at values",
    )
    parser.add_argument("--extend-policy", action="store_true")
    parser.add_argument("--policy-days", type=int, default=365)
    parser.add_argument("--conflicts", type=int, default=0)
    parser.add_argument("--conflict-lock-days", type=int, default=180)
    parser.add_argument("--timeout", type=float, default=120.0)
    parser.add_argument("--output", default=None)
    parser.add_argument("--quiet", action="store_true")
    parser.add_argument("--dry-run", action="store_true")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    if args.small_objects < 0 or args.medium_objects < 0:
        raise SystemExit("object counts must be non-negative")
    summary = generate(args)
    print(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
