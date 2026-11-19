"""Topology reconstruction, redundancy audit, and risk exposure calculation."""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

from .config import AppConfig
from .inventory import Inventory


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds")


def _connection(device_id: str, port: int) -> tuple[str, int]:
    return (device_id, int(port))


class Auditor:
    def __init__(self, config: AppConfig, inventory: Inventory) -> None:
        self.config = config
        self.inventory = inventory

    def build(self, batch_id: str, collection: dict[str, Any]) -> dict[str, Any]:
        started_at = utc_now()
        devices = collection["devices"]
        paths_by_server: dict[str, list[dict[str, Any]]] = {}
        discoverable = self._discovery_available(devices)
        for server_id in self.inventory.server_order:
            server = self.inventory.servers[server_id]
            paths_by_server[server_id] = self._server_paths(
                server, devices, discoverable=discoverable
            )

        server_results = [
            self._audit_server(
                self.inventory.servers[server_id],
                paths_by_server[server_id],
            )
            for server_id in sorted(self.inventory.server_order)
        ]
        device_results = self._device_exposure(server_results, devices)
        ranking = self._risk_ranking(device_results)
        unknown_devices = [
            self._device_summary(device)
            for device in device_results
            if not device["topology_known"]
        ]
        unknown_devices.sort(key=lambda item: item["device_id"])
        mismatches = [
            {
                "server_id": server["server_id"],
                "mismatches": server["declaration_mismatches"],
            }
            for server in server_results
            if server["declaration_mismatches"]
        ]
        status_counts = {
            "independent": 0,
            "pseudo_redundant": 0,
            "single_path": 0,
            "topology_unknown": 0,
        }
        for server in server_results:
            status_counts[server["redundancy_status"]] += 1
        online_devices = sum(1 for device in device_results if device["online"])
        totals = {
            "servers": len(server_results),
            "independent_servers": status_counts["independent"],
            "pseudo_redundant_servers": status_counts["pseudo_redundant"],
            "single_path_servers": status_counts["single_path"],
            "topology_unknown_servers": status_counts["topology_unknown"],
            "devices": len(device_results),
            "online_devices": online_devices,
            "topology_unknown_devices": len(unknown_devices),
            "declaration_mismatch_servers": len(mismatches),
        }
        completed_at = utc_now()
        return {
            "audit_version": 1,
            "batch_id": batch_id,
            "started_at": started_at,
            "completed_at": completed_at,
            "duration_ms": collection.get("duration_ms", 0.0),
            "collection_duration_ms": collection.get("duration_ms", 0.0),
            "within_total_deadline": collection.get("duration_ms", 0.0)
            <= self.config.total_timeout_seconds * 1000.0,
            "totals": totals,
            "servers": server_results,
            "devices": device_results,
            "risk_ranking": ranking,
            "unknown_devices": unknown_devices,
            "declaration_mismatches": mismatches,
        }

    @staticmethod
    def _discovery_available(devices: dict[str, dict[str, Any]]) -> bool:
        for device in devices.values():
            if device["device_type"] != "pdu" or not device["reachable"]:
                continue
            if any(
                port.get("connected_server_id")
                for port in device.get("ports", {}).values()
            ):
                return True
        return False

    def _path_from_port(
        self,
        server_id: str,
        pdu_id: str,
        port: int,
        declared_path_id: str | None,
        devices: dict[str, dict[str, Any]],
        *,
        declaration_only: bool = False,
    ) -> dict[str, Any]:
        pdu_result = devices.get(pdu_id)
        port_data: dict[str, Any] | None = None
        if pdu_result and pdu_result.get("reachable"):
            port_data = pdu_result.get("ports", {}).get(str(port))
        topology_reasons: list[str] = []
        pdu_topology_known = bool(pdu_result and pdu_result.get("topology_known"))
        if pdu_result is None:
            topology_reasons.append("PDU_NOT_IN_INVENTORY")
        elif not pdu_result.get("reachable"):
            topology_reasons.append("PDU_UNREACHABLE")
        elif not pdu_result.get("online"):
            topology_reasons.append("PDU_OFFLINE")
        if port_data is None:
            topology_reasons.append("PDU_PORT_MISSING")

        if declaration_only:
            actual_ups = None
            actual_branch = None
            port_status = "unknown"
            connected_server_id = None
            if pdu_result and pdu_result.get("reachable") and port_data:
                actual_ups = port_data.get("upstream_ups_id")
                actual_branch = port_data.get("upstream_branch_id")
                port_status = port_data.get("status", "unknown")
                connected_server_id = port_data.get("connected_server_id")
        else:
            actual_ups = None
            actual_branch = None
            port_status = "unknown"
            connected_server_id = None
            if port_data is not None:
                actual_ups = port_data.get("upstream_ups_id") or (
                    pdu_result.get("uplink_ups_id") if pdu_result else None
                )
                actual_branch = port_data.get("upstream_branch_id") or (
                    pdu_result.get("upstream_branch_id") if pdu_result else None
                )
                port_status = port_data.get("status", "unknown")
                connected_server_id = port_data.get("connected_server_id")
            if not actual_ups:
                topology_reasons.append("UPS_UPLINK_MISSING")
            if not actual_branch:
                topology_reasons.append("UPSTREAM_BRANCH_MISSING")

        ups_result = devices.get(actual_ups) if actual_ups else None
        ups_topology_known = bool(ups_result and ups_result.get("topology_known"))
        if actual_ups and ups_result is None:
            topology_reasons.append("UPS_NOT_IN_INVENTORY")
        elif actual_ups and not ups_topology_known:
            topology_reasons.append("UPS_UNREACHABLE")
        topology_known = bool(
            pdu_topology_known
            and ups_topology_known
            and actual_ups
            and actual_branch
            and not topology_reasons
        )
        return {
            "path_id": declared_path_id,  # Final A/B label is assigned later.
            "pdu_id": pdu_id,
            "port": port,
            "connected_server_id": connected_server_id,
            "ups_id": actual_ups,
            "upstream_branch_id": actual_branch,
            "port_status": port_status,
            "port_available": topology_known and port_status == "up",
            "topology_known": topology_known,
            "topology_reasons": sorted(set(topology_reasons)),
            "declared_path_id": declared_path_id,
            "source": "snmp_discovery" if not declaration_only else "declaration_unverified",
            "declared_ups_id": None,
            "declared_upstream_branch_id": None,
            "declaration_matches": None,
        }

    def _server_paths(
        self,
        server: dict[str, Any],
        devices: dict[str, dict[str, Any]],
        *,
        discoverable: bool,
    ) -> list[dict[str, Any]]:
        server_id = server["server_id"]
        declarations = server["power_inputs"]
        declared_by_key = {
            _connection(item["pdu_id"], item["port"]): item for item in declarations
        }
        paths: list[dict[str, Any]] = []
        discovered_keys: set[tuple[str, int]] = set()
        if discoverable:
            for pdu_id, pdu_result in devices.items():
                if pdu_result["device_type"] != "pdu" or not pdu_result["reachable"]:
                    continue
                for port_key, port in pdu_result.get("ports", {}).items():
                    if port.get("connected_server_id") != server_id:
                        continue
                    port_number = int(port_key)
                    key = _connection(pdu_id, port_number)
                    declared = declared_by_key.get(key)
                    path = self._path_from_port(
                        server_id,
                        pdu_id,
                        port_number,
                        declared["path_id"] if declared else None,
                        devices,
                    )
                    if declared:
                        path["declared_ups_id"] = declared["declared_ups_id"]
                        path["declared_upstream_branch_id"] = declared.get(
                            "declared_upstream_branch_id"
                        )
                        path["declaration_matches"] = self._declaration_matches(
                            declared, path
                        )
                    paths.append(path)
                    discovered_keys.add(key)

            for declared in declarations:
                key = _connection(declared["pdu_id"], declared["port"])
                if key in discovered_keys:
                    continue
                pdu_result = devices.get(declared["pdu_id"])
                if pdu_result is None or not pdu_result.get("reachable"):
                    path = self._path_from_port(
                        server_id,
                        declared["pdu_id"],
                        declared["port"],
                        declared["path_id"],
                        devices,
                        declaration_only=True,
                    )
                    path["declared_ups_id"] = declared["declared_ups_id"]
                    path["declared_upstream_branch_id"] = declared.get(
                        "declared_upstream_branch_id"
                    )
                    path["declaration_matches"] = False
                    paths.append(path)
                elif not paths:
                    # The PDU is reachable but does not attribute this port to
                    # this server. The declared connection is unverified.
                    path = self._path_from_port(
                        server_id,
                        declared["pdu_id"],
                        declared["port"],
                        declared["path_id"],
                        devices,
                        declaration_only=True,
                    )
                    path["declared_ups_id"] = declared["declared_ups_id"]
                    path["declared_upstream_branch_id"] = declared.get(
                        "declared_upstream_branch_id"
                    )
                    path["declaration_matches"] = False
                    path["topology_reasons"] = sorted(
                        set(path["topology_reasons"] + ["DECLARED_PORT_NOT_ASSIGNED"])
                    )
                    path["topology_known"] = False
                    path["port_available"] = False
                    paths.append(path)
        else:
            for declared in declarations:
                path = self._path_from_port(
                    server_id,
                    declared["pdu_id"],
                    declared["port"],
                    declared["path_id"],
                    devices,
                )
                path["source"] = "declaration_fallback"
                path["declared_ups_id"] = declared["declared_ups_id"]
                path["declared_upstream_branch_id"] = declared.get(
                    "declared_upstream_branch_id"
                )
                path["declaration_matches"] = self._declaration_matches(
                    declared, path
                )
                paths.append(path)
        self._assign_path_labels(paths)
        return paths

    @staticmethod
    def _declaration_matches(
        declared: dict[str, Any], path: dict[str, Any]
    ) -> bool:
        if path.get("declared_ups_id") is not None:
            return path["declared_ups_id"] == path.get("ups_id")
        if declared.get("declared_ups_id") != path.get("ups_id"):
            return False
        declared_branch = declared.get("declared_upstream_branch_id")
        if declared_branch and declared_branch != path.get("upstream_branch_id"):
            return False
        return True

    @staticmethod
    def _assign_path_labels(paths: list[dict[str, Any]]) -> None:
        paths.sort(
            key=lambda item: (
                item["pdu_id"],
                item["port"],
                item.get("ups_id") or "",
            )
        )
        used: set[str] = set()
        for path in paths:
            label = path.get("declared_path_id")
            if label and label not in used:
                path["path_id"] = label
                used.add(label)
        alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
        for index, path in enumerate(paths):
            if path.get("path_id"):
                continue
            label = alphabet[index] if index < len(alphabet) else f"P{index + 1}"
            while label in used:
                index += 1
                label = alphabet[index] if index < len(alphabet) else f"P{index + 1}"
            path["path_id"] = label
            used.add(label)
        paths.sort(key=lambda item: item["path_id"])

    def _audit_server(
        self, server: dict[str, Any], paths: list[dict[str, Any]]
    ) -> dict[str, Any]:
        known_paths = [path for path in paths if path["topology_known"]]
        all_known = len(known_paths) == len(paths)
        overlap_levels: list[str] = []
        reason_code = ""
        message = ""
        if not paths:
            status = "topology_unknown"
            reason_code = "NO_POWER_PATH"
            message = "No power path could be reconstructed."
        elif not all_known:
            status = "topology_unknown"
            reason_code = "PATH_TOPOLOGY_UNKNOWN"
            reasons = sorted(
                {
                    reason
                    for path in paths
                    if not path["topology_known"]
                    for reason in path["topology_reasons"]
                }
            )
            message = "At least one power path is unknown."
        elif len(paths) == 1:
            status = "single_path"
            reason_code = "SINGLE_POWER_PATH"
            message = "Only one power path is registered and verified."
        else:
            first, second = known_paths[0], known_paths[1]
            if first["pdu_id"] == second["pdu_id"]:
                overlap_levels.append("pdu")
            if first.get("ups_id") and first.get("ups_id") == second.get("ups_id"):
                overlap_levels.append("ups")
            if first.get("upstream_branch_id") and first.get(
                "upstream_branch_id"
            ) == second.get("upstream_branch_id"):
                overlap_levels.append("upstream_branch")
            if overlap_levels:
                status = "pseudo_redundant"
                primary = (
                    "pdu"
                    if "pdu" in overlap_levels
                    else "ups"
                    if "ups" in overlap_levels
                    else "upstream_branch"
                )
                reason_code = {
                    "pdu": "SAME_PDU",
                    "ups": "SAME_UPS",
                    "upstream_branch": "SAME_UPSTREAM_BRANCH",
                }[primary]
                message = "Power paths overlap at: " + ", ".join(overlap_levels) + "."
            else:
                status = "independent"
                reason_code = "INDEPENDENT_PATHS"
                message = "Power paths are independent at PDU, UPS, and upstream branch levels."

        primary_overlap = (
            "pdu"
            if "pdu" in overlap_levels
            else "ups"
            if "ups" in overlap_levels
            else "upstream_branch"
            if "upstream_branch" in overlap_levels
            else None
        )
        declarations = server["power_inputs"]
        actual_connections = [
            {
                "path_id": path["path_id"],
                "pdu_id": path["pdu_id"],
                "port": path["port"],
                "ups_id": path.get("ups_id"),
                "upstream_branch_id": path.get("upstream_branch_id"),
                "topology_known": path["topology_known"],
            }
            for path in paths
        ]
        declared_connections = [
            {
                "path_id": item["path_id"],
                "pdu_id": item["pdu_id"],
                "port": item["port"],
                "declared_ups_id": item["declared_ups_id"],
                "declared_upstream_branch_id": item.get(
                    "declared_upstream_branch_id"
                ),
            }
            for item in declarations
        ]
        mismatches = self._declaration_mismatches(
            server, paths, declarations, actual_connections
        )
        declaration_unverified = any(
            not path["topology_known"] for path in paths
        ) or len(paths) != len(declarations)
        declaration_match = not mismatches and not declaration_unverified
        available_path_count = sum(
            1 for path in paths if path["port_available"]
        )
        return {
            "server_id": server["server_id"],
            "rack": server.get("rack"),
            "business_cost": server["business_cost"],
            "acceptable_outage": server["acceptable_outage"],
            "redundancy_status": status,
            "is_redundant": status == "independent",
            "overlap_levels": overlap_levels,
            "overlap_level": primary_overlap,
            "reason_code": reason_code,
            "message": message,
            "power_path_count": len(paths),
            "available_path_count": available_path_count,
            "operationally_redundant": (
                status == "independent"
                and available_path_count == len(paths)
            ),
            "power_paths": paths,
            "declared_connections": declared_connections,
            "actual_connections": actual_connections,
            "declaration_match": declaration_match,
            "declaration_unverified": declaration_unverified,
            "declaration_mismatches": mismatches,
        }

    def _declaration_mismatches(
        self,
        server: dict[str, Any],
        paths: list[dict[str, Any]],
        declarations: list[dict[str, Any]],
        actual_connections: list[dict[str, Any]],
    ) -> list[dict[str, Any]]:
        mismatches: list[dict[str, Any]] = []
        actual_by_connection = {
            (item["pdu_id"], item["port"]): item for item in actual_connections
        }
        declared_keys = set()
        for declared in declarations:
            key = (declared["pdu_id"], declared["port"])
            declared_keys.add(key)
            actual = actual_by_connection.get(key)
            if actual is None:
                mismatches.append(
                    {
                        "path_id": declared["path_id"],
                        "kind": "DECLARED_CONNECTION_NOT_FOUND",
                        "declared_pdu_id": declared["pdu_id"],
                        "declared_port": declared["port"],
                        "actual_pdu_id": None,
                        "actual_port": None,
                    }
                )
                continue
            if actual.get("topology_known") and actual.get("ups_id") != declared["declared_ups_id"]:
                mismatches.append(
                    {
                        "path_id": declared["path_id"],
                        "kind": "DECLARED_UPS_MISMATCH",
                        "declared_ups_id": declared["declared_ups_id"],
                        "actual_ups_id": actual.get("ups_id"),
                    }
                )
            declared_branch = declared.get("declared_upstream_branch_id")
            if (
                actual.get("topology_known")
                and declared_branch
                and actual.get("upstream_branch_id") != declared_branch
            ):
                mismatches.append(
                    {
                        "path_id": declared["path_id"],
                        "kind": "DECLARED_BRANCH_MISMATCH",
                        "declared_upstream_branch_id": declared_branch,
                        "actual_upstream_branch_id": actual.get(
                            "upstream_branch_id"
                        ),
                    }
                )
        for actual in actual_connections:
            if (actual["pdu_id"], actual["port"]) not in declared_keys:
                mismatches.append(
                    {
                        "path_id": actual["path_id"],
                        "kind": "UNDECLARED_CONNECTION",
                        "actual_pdu_id": actual["pdu_id"],
                        "actual_port": actual["port"],
                    }
                )
        # Detect a declared port that the reachable PDU attributes elsewhere.
        for declared in declarations:
            key = (declared["pdu_id"], declared["port"])
            if key in actual_by_connection:
                continue
            for path in paths:
                if path["source"] == "declaration_unverified" and path["pdu_id"] == key[0] and path["port"] == key[1]:
                    if "DECLARED_CONNECTION_NOT_FOUND" not in {
                        item["kind"]
                        for item in mismatches
                        if item.get("path_id") == declared["path_id"]
                    }:
                        mismatches.append(
                            {
                                "path_id": declared["path_id"],
                                "kind": "DECLARED_CONNECTION_NOT_FOUND",
                                "declared_pdu_id": declared["pdu_id"],
                                "declared_port": declared["port"],
                                "actual_pdu_id": None,
                                "actual_port": None,
                            }
                        )
        unique: dict[tuple[Any, ...], dict[str, Any]] = {}
        for mismatch in mismatches:
            key = tuple(sorted((k, str(v)) for k, v in mismatch.items()))
            unique[key] = mismatch
        return [unique[key] for key in sorted(unique)]

    def _device_exposure(
        self,
        server_results: list[dict[str, Any]],
        collected: dict[str, dict[str, Any]],
    ) -> list[dict[str, Any]]:
        results: list[dict[str, Any]] = []
        for device_id in sorted(self.inventory.device_order):
            device = self.inventory.devices[device_id]
            collected_device = collected[device_id]
            device_type = device["device_type"]
            topology_known = bool(collected_device["topology_known"])
            unknown_reasons: list[str] = []
            if not collected_device["reachable"]:
                unknown_reasons.append(
                    collected_device.get("error_code") or "DEVICE_UNREACHABLE"
                )
            elif not collected_device["online"]:
                unknown_reasons.append("DEVICE_OFFLINE")
            if device_type == "pdu" and collected_device.get("uplink_ups_id"):
                uplink = collected.get(collected_device["uplink_ups_id"])
                if uplink is None or not uplink.get("topology_known"):
                    topology_known = False
                    unknown_reasons.append("UPLINK_UPS_UNKNOWN")
            if device_type == "pdu" and not collected_device.get("topology_known"):
                topology_known = False
                unknown_reasons.append("PDU_TOPOLOGY_INCOMPLETE")
            topology_known = topology_known and not unknown_reasons

            connected_servers: set[str] = set()
            all_out: list[str] = []
            one_path: list[str] = []
            uncertain: list[str] = []
            pseudo: list[str] = []
            single: list[str] = []
            for server in server_results:
                relevant_paths = [
                    path
                    for path in server["power_paths"]
                    if (
                        path.get("pdu_id") == device_id
                        if device_type == "pdu"
                        else path.get("ups_id") == device_id
                    )
                ]
                declared_relevant = [
                    item
                    for item in server["declared_connections"]
                    if (
                        item["pdu_id"] == device_id
                        if device_type == "pdu"
                        else item["declared_ups_id"] == device_id
                    )
                ]
                if relevant_paths:
                    connected_servers.add(server["server_id"])
                attached = bool(relevant_paths) or (
                    not topology_known and bool(declared_relevant)
                )
                if server["redundancy_status"] == "pseudo_redundant" and attached:
                    pseudo.append(server["server_id"])
                if server["redundancy_status"] == "single_path" and attached:
                    single.append(server["server_id"])
                if not topology_known:
                    if declared_relevant:
                        uncertain.append(server["server_id"])
                    continue
                known_paths = [
                    path for path in server["power_paths"] if path["topology_known"]
                ]
                has_unknown_path = len(known_paths) != len(server["power_paths"])
                if not relevant_paths:
                    continue
                if has_unknown_path:
                    uncertain.append(server["server_id"])
                    continue
                if len(relevant_paths) == len(known_paths):
                    all_out.append(server["server_id"])
                else:
                    one_path.append(server["server_id"])
            output = {
                "device_id": device_id,
                "device_type": device_type,
                "online": bool(collected_device["online"]),
                "reachable": bool(collected_device["reachable"]),
                "status": collected_device["status"],
                "topology_known": topology_known,
                "topology_unknown_reasons": sorted(set(unknown_reasons)),
                "server_count": len(connected_servers | set(uncertain)),
                "all_outage_count": len(all_out),
                "one_path_loss_count": len(one_path),
                "pseudo_redundant_count": len(pseudo),
                "single_path_count": len(single),
                "all_outage_servers": sorted(all_out),
                "one_path_loss_servers": sorted(one_path),
                "pseudo_redundant_servers": sorted(pseudo),
                "single_path_servers": sorted(single),
                "uncertain_server_ids": sorted(set(uncertain)),
                "declared_uplink_ups_id": (
                    collected_device.get("uplink_ups_id")
                    if device_type == "pdu"
                    else None
                ),
                "output_load_percent": collected_device.get("output_load_percent"),
                "battery_remaining_percent": collected_device.get(
                    "battery_remaining_percent"
                ),
                "runtime_minutes": collected_device.get("runtime_minutes"),
                "supply_state": collected_device.get("supply_state", "unknown"),
            }
            results.append(output)
        return results

    @staticmethod
    def _risk_ranking(devices: list[dict[str, Any]]) -> list[dict[str, Any]]:
        ranked = [device for device in devices if device["topology_known"]]
        ranked.sort(
            key=lambda item: (
                -item["all_outage_count"],
                -item["one_path_loss_count"],
                item["device_id"],
            )
        )
        return [
            {
                "rank": index + 1,
                "device_id": device["device_id"],
                "device_type": device["device_type"],
                "all_outage_count": device["all_outage_count"],
                "one_path_loss_count": device["one_path_loss_count"],
                "server_count": device["server_count"],
                "all_outage_servers": device["all_outage_servers"],
                "one_path_loss_servers": device["one_path_loss_servers"],
            }
            for index, device in enumerate(ranked)
        ]

    @staticmethod
    def _device_summary(device: dict[str, Any]) -> dict[str, Any]:
        return {
            "device_id": device["device_id"],
            "device_type": device["device_type"],
            "online": device["online"],
            "status": device["status"],
            "reasons": device["topology_unknown_reasons"],
            "uncertain_server_ids": device["uncertain_server_ids"],
        }
