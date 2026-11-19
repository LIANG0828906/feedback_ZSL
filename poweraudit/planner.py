"""Maintenance shutdown plans and low-battery emergency sequences."""

from __future__ import annotations

import uuid
from typing import Any

from .audit import utc_now
from .config import AppConfig


class PlanError(ValueError):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


def _path_affected_by(path: dict[str, Any], target_type: str, target_id: str) -> bool:
    if target_type == "pdu":
        return path.get("pdu_id") == target_id
    return path.get("ups_id") == target_id


def _declared_affected_by(
    declared: dict[str, Any], target_type: str, target_id: str
) -> bool:
    if target_type == "pdu":
        return declared.get("pdu_id") == target_id
    return declared.get("declared_ups_id") == target_id


class Planner:
    def __init__(self, config: AppConfig) -> None:
        self.config = config

    def maintenance_plan(
        self,
        audit: dict[str, Any],
        target_type: str,
        target_id: str,
        *,
        acceptable_server_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        target_type = target_type.lower()
        if target_type not in ("ups", "pdu"):
            raise PlanError("INVALID_TARGET_TYPE", "target_type must be ups or pdu")
        devices = {item["device_id"]: item for item in audit["devices"]}
        target = devices.get(target_id)
        if target is None or target["device_type"] != target_type:
            raise PlanError("UNKNOWN_DEVICE", f"No {target_type} {target_id} in audit batch")
        acceptable = set(acceptable_server_ids or [])
        full_outage: list[dict[str, Any]] = []
        one_path: list[dict[str, Any]] = []
        uncertain: list[dict[str, Any]] = []
        plan_id = f"PLAN-{uuid.uuid4()}"

        for server in audit["servers"]:
            declared = server["declared_connections"]
            paths = server["power_paths"]
            known_paths = [path for path in paths if path["topology_known"]]
            unknown_paths = [path for path in paths if not path["topology_known"]]
            affected_known = [
                path
                for path in known_paths
                if _path_affected_by(path, target_type, target_id)
            ]
            uncertain_paths = [
                path
                for path in unknown_paths
                if _path_affected_by(path, target_type, target_id)
                or any(
                    _declared_affected_by(item, target_type, target_id)
                    for item in declared
                    if item["path_id"] == path["path_id"]
                )
            ]
            declared_targets = [
                item
                for item in declared
                if _declared_affected_by(item, target_type, target_id)
            ]
            if not target["topology_known"]:
                if affected_known or declared_targets or uncertain_paths:
                    uncertain.append(
                        self._uncertain_server(
                          server,
                          "TARGET_DEVICE_TOPOLOGY_UNKNOWN",
                          [item["path_id"] for item in affected_known],
                        )
                    )
                continue
            if uncertain_paths:
                uncertain.append(
                    self._uncertain_server(
                        server,
                        "SERVER_PATH_TOPOLOGY_UNKNOWN",
                        [item["path_id"] for item in uncertain_paths],
                    )
                )
                continue
            if not affected_known:
                continue
            relevant_paths = sorted(
                {path["path_id"] for path in affected_known}
            )
            unaffected_paths = sorted(
                {
                    path["path_id"]
                    for path in known_paths
                    if not _path_affected_by(path, target_type, target_id)
                }
            )
            is_waivable = server["acceptable_outage"] or server["server_id"] in acceptable
            if len(affected_known) == len(known_paths):
                item = {
                    "server_id": server["server_id"],
                    "impact": "full_outage",
                    "affected_paths": relevant_paths,
                    "unaffected_paths": [],
                    "reason_code": "ALL_POWER_PATHS_AFFECTED_BY_TARGET",
                    "reason": "All known power paths depend on the shutdown target.",
                    "business_cost": server["business_cost"],
                    "acceptable_outage": is_waivable,
                }
                full_outage.append(item)
            else:
                item = {
                    "server_id": server["server_id"],
                    "impact": "one_path_loss",
                    "affected_paths": relevant_paths,
                    "unaffected_paths": unaffected_paths,
                    "reason_code": "ONE_POWER_PATH_AFFECTED",
                    "reason": "At least one independent power path remains.",
                    "business_cost": server["business_cost"],
                    "acceptable_outage": is_waivable,
                }
                one_path.append(item)

        full_outage.sort(key=lambda item: (item["server_id"]))
        one_path.sort(key=lambda item: (item["server_id"]))
        uncertain.sort(key=lambda item: item["server_id"])
        planned = full_outage + one_path
        blocked = [
            item["server_id"] for item in full_outage if not item["acceptable_outage"]
        ]
        waived = [
            item["server_id"]
            for item in full_outage
            if item["acceptable_outage"]
        ]
        blocking_reasons: list[dict[str, Any]] = []
        if blocked:
            blocking_reasons.append(
                {
                    "code": "UNAPPROVED_FULL_OUTAGE",
                    "message": "Some servers would lose all power and are not on the acceptable interruption list.",
                    "server_ids": sorted(blocked),
                }
            )
        if uncertain:
            blocking_reasons.append(
                {
                    "code": "TOPOLOGY_UNKNOWN",
                    "message": "Some servers have unverified paths that could be affected by the target.",
                    "server_ids": [item["server_id"] for item in uncertain],
                }
            )
        if not target["topology_known"]:
            blocking_reasons.append(
                {
                    "code": "TARGET_TOPOLOGY_UNKNOWN",
                    "message": "The shutdown target is unreachable or offline; downstream impact is unknown.",
                    "device_id": target_id,
                }
            )
        return {
            "plan_version": 1,
            "plan_id": plan_id,
            "batch_id": audit["batch_id"],
            "plan_type": "maintenance",
            "created_at": utc_now(),
            "target": {"type": target_type, "id": target_id},
            "executable": not blocking_reasons,
            "blocking_reasons": blocking_reasons,
            "blocked_servers": sorted(blocked),
            "waivable_servers": sorted(waived),
            "counts": {
                "full_outage": len(full_outage),
                "one_path_loss": len(one_path),
                "planned_total": len(planned),
                "unknown_impact": len(uncertain),
            },
            "servers": planned,
            "unknown_impact_servers": uncertain,
        }

    @staticmethod
    def _uncertain_server(
        server: dict[str, Any], reason_code: str, path_ids: list[str]
    ) -> dict[str, Any]:
        return {
            "server_id": server["server_id"],
            "reason_code": reason_code,
            "affected_or_unknown_paths": sorted(set(path_ids)),
        }

    def emergency_plans(
        self,
        audit: dict[str, Any],
        *,
        ups_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        devices = {item["device_id"]: item for item in audit["devices"]}
        ups_devices = {
            device_id: device
            for device_id, device in devices.items()
            if device["device_type"] == "ups"
        }
        threshold = self.config.emergency_battery_threshold_percent
        runtime_threshold = self.config.emergency_runtime_threshold_minutes
        requested = sorted(set(ups_ids or []))
        low_ids: list[str] = []
        candidates = requested if requested else sorted(ups_devices)
        for ups_id in candidates:
            ups = ups_devices.get(ups_id)
            if ups is None:
                raise PlanError("UNKNOWN_DEVICE", f"No UPS {ups_id} in audit batch")
            battery = ups.get("battery_remaining_percent")
            supply = ups.get("supply_state")
            runtime = ups.get("runtime_minutes")
            if (
                supply == "battery"
                and battery is not None
                and battery < threshold
            ):
                low_ids.append(ups_id)
        if not low_ids:
            raise PlanError(
                "NO_EMERGENCY_CONDITION",
                "No selected UPS is on battery below the configured capacity threshold.",
            )
        low_set = set(low_ids)
        candidates_with_positions: list[dict[str, Any]] = []
        uncertain: list[dict[str, Any]] = []
        for server in audit["servers"]:
            paths = server["power_paths"]
            known_paths = [path for path in paths if path["topology_known"]]
            unknown_paths = [path for path in paths if not path["topology_known"]]
            if unknown_paths:
                relevant_unknown = [
                    path
                    for path in unknown_paths
                    if path.get("ups_id") in low_set
                    or any(
                        item.get("declared_ups_id") in low_set
                        for item in server["declared_connections"]
                        if item["path_id"] == path["path_id"]
                    )
                ]
                if relevant_unknown:
                    uncertain.append(
                        {
                            "server_id": server["server_id"],
                            "reason_code": "PATH_TOPOLOGY_UNKNOWN",
                            "path_ids": sorted(path["path_id"] for path in relevant_unknown),
                        }
                    )
                    continue
            failing_paths = [
                path for path in known_paths if path.get("ups_id") in low_set
            ]
            if not failing_paths:
                continue
            remaining_paths = [
                path for path in known_paths if path.get("ups_id") not in low_set
            ]
            if not remaining_paths:
                priority = 1
                priority_label = (
                    "SINGLE_PATH_ON_FAILING_UPS"
                    if len(known_paths) == 1
                    else "ALL_PATHS_ON_FAILING_UPS"
                )
                reason_code = (
                    "SINGLE_PATH_ON_FAILING_UPS"
                    if len(known_paths) == 1
                    else "ALL_KNOWN_PATHS_ON_FAILING_UPS"
                )
                reason = (
                    "The server has no independent path away from the failing UPS."
                )
                alternates: list[dict[str, Any]] = []
                transfer_load = self._server_transfer_load(
                    failing_paths, ups_devices, server
                )
            else:
                alternates = [
                    self._alternate_ups_assessment(
                        path,
                        ups_devices,
                        transfer_load=self._server_transfer_load(
                            failing_paths, ups_devices, server
                        ),
                    )
                    for path in remaining_paths
                ]
                if any(item["can_carry"] for item in alternates):
                    continue
                priority = 2
                priority_label = "FAILOVER_CAPACITY_INSUFFICIENT"
                reason_code = "ALTERNATE_UPS_CAPACITY_INSUFFICIENT"
                reason = "At least one alternate path exists, but no alternate UPS can accept the transfer load."
            primary_failing = sorted(
                {
                    path.get("ups_id")
                    for path in failing_paths
                    if path.get("ups_id")
                }
            )
            runtime_values = [
                ups_devices[ups_id].get("runtime_minutes")
                for ups_id in primary_failing
                if ups_id in ups_devices
            ]
            candidates_with_positions.append(
                {
                    "server_id": server["server_id"],
                    "business_cost": server["business_cost"],
                    "priority": priority,
                    "priority_label": priority_label,
                    "reason_code": reason_code,
                    "reason": reason,
                    "failing_ups_ids": primary_failing,
                    "affected_paths": sorted(path["path_id"] for path in failing_paths),
                    "alternate_ups": alternates,
                    "remaining_runtime_minutes": min(
                        [value for value in runtime_values if value is not None],
                        default=None,
                    ),
                    "basis": {
                        "business_cost": server["business_cost"],
                        "tier": priority,
                        "battery_threshold_percent": threshold,
                        "runtime_threshold_minutes": runtime_threshold,
                    },
                }
            )
        candidates_with_positions.sort(
            key=lambda item: (
                item["priority"],
                item["business_cost"],
                item["server_id"],
            )
        )
        sequence: list[dict[str, Any]] = []
        for index, item in enumerate(candidates_with_positions, start=1):
            ordered = dict(item)
            ordered["position"] = index
            sequence.append(ordered)
        return {
            "plan_version": 1,
            "plan_id": f"EMERG-{uuid.uuid4()}",
            "batch_id": audit["batch_id"],
            "plan_type": "emergency",
            "created_at": utc_now(),
            "trigger": {
                "ups_ids": low_ids,
                "condition": "on_battery_below_threshold",
                "battery_threshold_percent": threshold,
                "runtime_threshold_minutes": runtime_threshold,
            },
            "executable": True,
            "blocking_reasons": [],
            "counts": {
                "priority_1": sum(1 for item in sequence if item["priority"] == 1),
                "priority_2": sum(1 for item in sequence if item["priority"] == 2),
                "planned_total": len(sequence),
                "unknown_impact": len(uncertain),
            },
            "sequence": sequence,
            "unknown_impact_servers": sorted(
                uncertain, key=lambda item: item["server_id"]
            ),
            "ordering_rule": (
                "priority ascending, business_cost ascending, server_id ascending"
            ),
        }

    @staticmethod
    def _server_transfer_load(
        failing_paths: list[dict[str, Any]],
        ups_devices: dict[str, dict[str, Any]],
        server: dict[str, Any],
    ) -> float:
        ups_id = failing_paths[0].get("ups_id") if failing_paths else None
        ups = ups_devices.get(ups_id, {}) if ups_id else {}
        load = ups.get("output_load_percent")
        server_count = ups.get("server_count") or 1
        if isinstance(load, (int, float)):
            return round(max(1.0, float(load) / max(1, int(server_count))), 2)
        return 1.0

    def _alternate_ups_assessment(
        self,
        path: dict[str, Any],
        ups_devices: dict[str, dict[str, Any]],
        *,
        transfer_load: float,
    ) -> dict[str, Any]:
        ups_id = path.get("ups_id")
        ups = ups_devices.get(ups_id, {}) if ups_id else {}
        load = ups.get("output_load_percent")
        battery = ups.get("battery_remaining_percent")
        runtime = ups.get("runtime_minutes")
        reserve = self.config.failover_reserve_percent
        can_carry = True
        reasons: list[str] = []
        if not ups or not ups.get("topology_known") or not ups.get("online"):
            can_carry = False
            reasons.append("ALTERNATE_UPS_UNKNOWN")
        if isinstance(load, (int, float)):
            projected = float(load) + max(
                transfer_load, self.config.min_transfer_load_percent
            )
            if projected > 100.0 - reserve:
                can_carry = False
                reasons.append("INSUFFICIENT_LOAD_HEADROOM")
        else:
            can_carry = False
            reasons.append("ALTERNATE_UPS_LOAD_UNKNOWN")
        if ups.get("supply_state") == "battery":
            if battery is None or battery < self.config.emergency_battery_threshold_percent:
                can_carry = False
                reasons.append("ALTERNATE_UPS_BATTERY_LOW")
            if (
                runtime is None
                or runtime < self.config.emergency_runtime_threshold_minutes
            ):
                can_carry = False
                reasons.append("ALTERNATE_UPS_RUNTIME_LOW")
        headroom = (
            round(100.0 - float(load) - reserve, 2)
            if isinstance(load, (int, float))
            else None
        )
        return {
            "ups_id": ups_id,
            "path_id": path["path_id"],
            "can_carry": can_carry,
            "reasons": sorted(set(reasons)),
            "current_load_percent": load,
            "battery_remaining_percent": battery,
            "runtime_minutes": runtime,
            "transfer_load_percent": round(
                max(transfer_load, self.config.min_transfer_load_percent), 2
            ),
            "reserve_percent": reserve,
            "load_headroom_percent": headroom,
        }

    @staticmethod
    def server_position(plan: dict[str, Any], server_id: str) -> dict[str, Any] | None:
        if plan["plan_type"] != "emergency":
            return None
        for item in plan["sequence"]:
            if item["server_id"] == server_id:
                return {
                    "plan_id": plan["plan_id"],
                    "batch_id": plan["batch_id"],
                    "server_id": server_id,
                    "position": item["position"],
                    "total": len(plan["sequence"]),
                    "priority": item["priority"],
                    "priority_label": item["priority_label"],
                    "reason_code": item["reason_code"],
                    "reason": item["reason"],
                    "basis": item["basis"],
                    "failing_ups_ids": item["failing_ups_ids"],
                    "affected_paths": item["affected_paths"],
                    "alternate_ups": item["alternate_ups"],
                }
        return None


def plan_summary(plan: dict[str, Any]) -> dict[str, Any]:
    return {
        "plan_id": plan["plan_id"],
        "batch_id": plan["batch_id"],
        "plan_type": plan["plan_type"],
        "created_at": plan["created_at"],
        "target": plan.get("target"),
        "executable": plan["executable"],
        "counts": plan["counts"],
        "trigger": plan.get("trigger"),
        "blocking_reason_codes": [
            item["code"] for item in plan.get("blocking_reasons", [])
        ],
    }
