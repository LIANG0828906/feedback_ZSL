from __future__ import annotations

import asyncio
import math
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable

from .util import atomic_write_json, deterministic_bucket, read_json, utc_now


TYPE_NONE = "none"
TYPE_SYN_DROP = "syn_drop"
TYPE_UPSTREAM_DISCONNECT = "upstream_disconnect"
TYPE_PROXY_READ_TIMEOUT = "proxy_read_timeout"

KNOWN_TYPES = {
    TYPE_NONE,
    TYPE_SYN_DROP,
    TYPE_UPSTREAM_DISCONNECT,
    TYPE_PROXY_READ_TIMEOUT,
}

EXPECTED_HOP = {
    TYPE_NONE: "none",
    TYPE_SYN_DROP: "proxy_to_upstream",
    TYPE_UPSTREAM_DISCONNECT: "upstream",
    TYPE_PROXY_READ_TIMEOUT: "proxy_upstream_read",
}

PATH_VALUES = {"all", "short", "keepalive", "upload"}


class InjectionValidationError(Exception):
    code = "INVALID_INJECTION"
    http_status = 400
    message = "invalid injection configuration"


def _parse_iso_ms(value: str | None) -> int | None:
    if not value:
        return None
    text = value.replace("Z", "+00:00")
    return int(datetime.fromisoformat(text).timestamp() * 1000)


def _epoch_ms() -> int:
    return int(datetime.now(timezone.utc).timestamp() * 1000)


def normalize_injection(raw: dict[str, Any], *, generate_id: bool = True) -> dict[str, Any]:
    if not isinstance(raw, dict):
        raise InjectionValidationError("injection must be a JSON object")
    injection_type = str(
        raw.get("type", raw.get("fault_type", raw.get("action", "")))
    ).strip()
    if injection_type not in KNOWN_TYPES:
        raise InjectionValidationError(
            f"type must be one of {sorted(KNOWN_TYPES)}"
        )
    path = str(raw.get("path", raw.get("scope_path", "all"))).strip()
    if path not in PATH_VALUES:
        raise InjectionValidationError(f"path must be one of {sorted(PATH_VALUES)}")
    try:
        ratio = float(raw.get("ratio", raw.get("probability", 1.0)))
    except (TypeError, ValueError) as exc:
        raise InjectionValidationError("ratio must be a number between 0 and 1") from exc
    if not 0.0 <= ratio <= 1.0:
        raise InjectionValidationError("ratio must be between 0 and 1")
    try:
        duration_ms = int(
            raw.get(
                "duration_ms",
                float(raw.get("duration_seconds", 300)) * 1000,
            )
        )
    except (TypeError, ValueError) as exc:
        raise InjectionValidationError("duration_ms must be a positive integer") from exc
    if duration_ms <= 0:
        raise InjectionValidationError("duration_ms must be positive")

    hop = raw.get("hop", raw.get("target_hop"))
    if hop is None:
        hop = EXPECTED_HOP[injection_type]
    hop = str(hop)
    if injection_type == TYPE_NONE:
        hop = "none"
    elif hop != EXPECTED_HOP[injection_type]:
        raise InjectionValidationError(
            f"hop for {injection_type} must be {EXPECTED_HOP[injection_type]}"
        )

    activated_at = raw.get("activated_at") or utc_now()
    activated_ms = _parse_iso_ms(str(activated_at))
    if activated_ms is None:
        raise InjectionValidationError("activated_at must be an ISO-8601 timestamp")
    expires_at = raw.get("expires_at")
    expires_ms = _parse_iso_ms(str(expires_at)) if expires_at else activated_ms + duration_ms
    if expires_ms <= activated_ms:
        raise InjectionValidationError("injection expiry must be after activation")

    injection_id = str(raw.get("id") or "").strip()
    if not injection_id and generate_id:
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%f")
        injection_id = f"inj-{stamp}-{injection_type}"
    if not injection_id:
        raise InjectionValidationError("id must not be empty")

    params = raw.get("params", {})
    if not isinstance(params, dict):
        raise InjectionValidationError("params must be an object")

    return {
        "id": injection_id,
        "type": injection_type,
        "hop": hop,
        "duration_ms": duration_ms,
        "ratio": ratio,
        "path": path,
        "activated_at": datetime.fromtimestamp(
            activated_ms / 1000.0, timezone.utc
        ).isoformat(timespec="milliseconds").replace("+00:00", "Z"),
        "expires_at": datetime.fromtimestamp(
            expires_ms / 1000.0, timezone.utc
        ).isoformat(timespec="milliseconds").replace("+00:00", "Z"),
        "seed": int(raw.get("seed", 318)),
        "params": params,
    }


class InjectionPlan:
    def __init__(
        self,
        profile: dict[str, Any],
        batch_id: str,
        request_plans: Iterable[dict[str, Any]],
        now_ms: int,
    ) -> None:
        self.profile = profile
        self.batch_id = batch_id
        active = (
            profile["type"] != TYPE_NONE
            and _parse_iso_ms(profile["activated_at"]) <= now_ms < _parse_iso_ms(profile["expires_at"])
        )
        self.active = active
        eligible: list[dict[str, Any]] = []
        if active:
            for plan in request_plans:
                plan_path = plan["path"] if isinstance(plan, dict) else plan.path
                if profile["path"] == "all" or profile["path"] == plan_path:
                    eligible.append(plan)
        eligible.sort(
            key=lambda plan: deterministic_bucket(
                profile["seed"],
                profile["id"],
                batch_id,
                plan["request_id"] if isinstance(plan, dict) else plan.request_id,
            )
        )
        quota = int(math.floor(len(eligible) * profile["ratio"] + 0.5))
        if profile["ratio"] > 0 and eligible and quota == 0:
            quota = 1
        self.eligible_request_ids = [
            plan["request_id"] if isinstance(plan, dict) else plan.request_id
            for plan in eligible
        ]
        self.matched_request_ids = {
            plan["request_id"] if isinstance(plan, dict) else plan.request_id
            for plan in eligible[: max(0, min(quota, len(eligible)))]
        }

    def applies(self, request_id: str) -> bool:
        return self.active and request_id in self.matched_request_ids

    def snapshot(self) -> dict[str, Any]:
        return {
            "active": self.active,
            "eligible_count": len(self.eligible_request_ids),
            "eligible_request_ids": sorted(self.eligible_request_ids),
            "matched_count": len(self.matched_request_ids),
            "matched_request_ids": sorted(self.matched_request_ids),
            "profile": self.profile,
            "selection": "deterministic_hash_quota",
        }


class InjectionController:
    def __init__(self, path: str | Path, default_id: str = "baseline") -> None:
        self.path = Path(path)
        self.default_id = default_id
        self._lock = asyncio.Lock()
        self.path.parent.mkdir(parents=True, exist_ok=True)
        if not self.path.exists():
            profile = normalize_injection(
                {
                    "id": default_id,
                    "type": TYPE_NONE,
                    "duration_ms": 31_536_000_000,
                    "ratio": 0.0,
                    "path": "all",
                }
            )
            atomic_write_json(self.path, profile)

    def current(self) -> dict[str, Any]:
        raw = read_json(self.path)
        if not isinstance(raw, dict):
            raise InjectionValidationError("injection file must contain one object")
        return normalize_injection(raw, generate_id=False)

    async def replace(self, raw: dict[str, Any]) -> dict[str, Any]:
        profile = normalize_injection(raw)
        async with self._lock:
            atomic_write_json(self.path, profile)
        return profile

    def plan(
        self,
        *,
        batch_id: str,
        request_plans: Iterable[dict[str, Any]],
        requested_id: str | None = None,
        now_ms: int | None = None,
    ) -> InjectionPlan:
        profile = self.current()
        if requested_id and requested_id != profile["id"]:
            raise InjectionValidationError(
                f"requested injection {requested_id!r} is not the active injection "
                f"{profile['id']!r}"
            )
        return InjectionPlan(
            profile,
            batch_id,
            request_plans,
            now_ms if now_ms is not None else _epoch_ms(),
        )
