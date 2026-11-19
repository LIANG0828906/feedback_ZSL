#!/usr/bin/env python3
from __future__ import annotations

import argparse
import copy
import json
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from oob_rack.collector import RackCollector
from oob_rack.config import ConfigManager, resolve_path
from oob_rack.control import ControlServer
from oob_rack.deterministic import deterministic_power
from oob_rack.emulator import BmcEmulator
from oob_rack.emulator_state import FaultController
from oob_rack.generate import build_config
from oob_rack.monitor import MonitorServer
from oob_rack.service import InspectorService
from oob_rack.storage import RackStorage, utc_now


def http_json(method: str, url: str, body: dict | None = None) -> tuple[int, dict]:
    data = None if body is None else json.dumps(body).encode("utf-8")
    request = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        request.add_header("Content-Type", "application/json")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(request, timeout=10) as response:
            return response.status, json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


def http_text(url: str) -> str:
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(url, timeout=10) as response:
        return response.read().decode("utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description="Run the mixed-firmware acceptance scenario.")
    parser.add_argument(
        "--output", default=str(ROOT / "artifacts" / "acceptance-report.json")
    )
    parser.add_argument("--high-round-delay", type=float, default=2.0)
    args = parser.parse_args()

    runtime_config_path = ROOT / "artifacts" / "acceptance-config.json"
    runtime_db_path = ROOT / "artifacts" / "acceptance-inspections.db"
    for path in (
        runtime_config_path,
        runtime_db_path,
        Path(str(runtime_db_path) + "-wal"),
        Path(str(runtime_db_path) + "-shm"),
    ):
        if path.exists():
            path.unlink()

    config = build_config(2872026)
    config["inspection"]["database"] = runtime_db_path.name
    config["monitor"]["port"] = 9470
    config["control"]["port"] = 9471
    runtime_config_path.write_text(
        json.dumps(config, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )

    manager = ConfigManager(runtime_config_path)
    storage = RackStorage(resolve_path(manager.config, manager.config["inspection"]["database"]))
    faults = FaultController()
    emulator = BmcEmulator(manager.config, faults)
    control = ControlServer("127.0.0.1", 9471, faults)
    service = InspectorService(manager, storage)
    monitor = MonitorServer("127.0.0.1", 9470, service)
    checks: list[dict] = []
    report: dict = {}

    def check(name: str, condition: bool, details=None) -> None:
        checks.append(
            {
                "name": name,
                "passed": bool(condition),
                "details": details,
            }
        )

    try:
        emulator.start()
        control.start()
        monitor.start()

        baseline_result = service.run_once()
        baseline = storage.latest_status(manager.config["rack_id"])
        baseline_detail = storage.inspection_detail(baseline_result["inspection_id"])
        check(
            "six_devices_collected",
            len(baseline["devices"]) == 6
            and all(
                item["status"] in ("ok", "degraded")
                for item in baseline["devices"]
            ),
            {"devices": [item["device_id"] for item in baseline["devices"]]},
        )
        check(
            "mixed_firmware_protocols",
            next(item for item in baseline["devices"] if item["device_id"] == "bmc-01")[
                "protocols_used"
            ]
            == ["ipmi", "snmp"]
            and "redfish" in baseline["devices"][1]["protocols_used"]
            and "redfish" in baseline["devices"][2]["protocols_used"],
        )
        check(
            "normalized_values_have_bounds",
            all(
                sensor["unit"] in ("C", "RPM")
                and isinstance(sensor["value"], (int, float))
                and sensor["lower"] < sensor["upper"]
                for device in baseline_detail["devices"]
                for sensor in device["normalized"]["sensors"]
            ),
        )
        check(
            "raw_and_normalized_available_together",
            all(
                device["protocols"]
                and all("raw" in protocol for protocol in device["protocols"])
                and device["normalized"].get("sensors")
                for device in baseline_detail["devices"]
            ),
        )
        check(
            "no_false_cross_protocol_mismatch_at_baseline",
            baseline["inconsistencies"] == [],
        )

        # Interrupted inspection: commit one device, then resume in a fresh service cycle.
        partial = storage.start_or_resume_inspection(
            rack_id=manager.config["rack_id"],
            fingerprint=manager.fingerprint,
            config_version=manager.generation,
            expected_devices=6,
        )
        first_aggregate = RackCollector(manager.config).collect_device(
            manager.config["devices"][0]
        )
        storage.record_device_result(
            partial["inspection_id"], first_aggregate, observed_at=utc_now()
        )
        resumed_result = service.run_once()
        resumed_detail = storage.inspection_detail(resumed_result["inspection_id"])
        check(
            "inspection_resumes_after_restart",
            resumed_result["resumed"]
            and len(storage.committed_device_ids(resumed_result["inspection_id"])) == 6
            and len(resumed_detail["devices"]) == 6,
            {"inspection_id": resumed_result["inspection_id"]},
        )

        status, _ = http_json(
            "POST",
            "http://127.0.0.1:9471/faults",
            {"device_id": "bmc-02", "kind": "offline", "protocol": "redfish"},
        )
        fallback_result = service.run_once()
        fallback_status = storage.latest_status(manager.config["rack_id"])
        fallback_device = next(
            item for item in fallback_status["devices"] if item["device_id"] == "bmc-02"
        )
        fallback_detail = storage.inspection_detail(fallback_result["inspection_id"])
        fallback_protocol = next(
            item
            for item in fallback_detail["devices"]
            if item["device_id"] == "bmc-02"
        )
        redfish_result = next(
            item
            for item in fallback_protocol["protocols"]
            if item["protocol"] == "redfish"
        )
        check(
            "fallback_is_not_marked_unsupported",
            status == 201
            and fallback_device["fallback_used"]
            and fallback_device["primary_protocol"] == "ipmi"
            and not redfish_result["success"]
            and "unsupported" not in redfish_result,
            {
                "primary_protocol": fallback_device["primary_protocol"],
                "protocols_used": fallback_device["protocols_used"],
            },
        )
        http_json("DELETE", "http://127.0.0.1:9471/faults?device_id=bmc-02")

        http_json(
            "POST",
            "http://127.0.0.1:9471/faults",
            {"device_id": "bmc-05", "kind": "redfish_power_unparsable"},
        )
        unparsable_result = service.run_once()
        unparsable_status = storage.latest_status(manager.config["rack_id"])
        unparsable_device = next(
            item for item in unparsable_status["devices"] if item["device_id"] == "bmc-05"
        )
        unparsable_items = [
            item for item in unparsable_status["unparsable"] if item["device_id"] == "bmc-05"
        ]
        check(
            "unparsable_shape_is_degraded_not_offline",
            unparsable_device["status"] == "degraded"
            and unparsable_device["unparsable_fields"] >= 1
            and unparsable_items
            and unparsable_items[0]["raw_snippet"],
            {"inspection_id": unparsable_result["inspection_id"]},
        )
        http_json("DELETE", "http://127.0.0.1:9471/faults?device_id=bmc-05")

        base_power = deterministic_power(
            manager.config["seed"], "bmc-03", "new-3.0"
        )
        opposite = "off" if base_power == "on" else "on"
        http_json(
            "POST",
            "http://127.0.0.1:9471/faults",
            {
                "device_id": "bmc-03",
                "kind": "power_override",
                "protocol": "ipmi",
                "value": opposite,
            },
        )
        mismatch_result = service.run_once()
        mismatch_status = storage.latest_status(manager.config["rack_id"])
        mismatch_items = [
            item
            for item in mismatch_status["inconsistencies"]
            if item["device_id"] == "bmc-03" and item["item"] == "power_state"
        ]
        check(
            "cross_protocol_mismatch_has_both_raw_values",
            len(mismatch_items) == 1
            and mismatch_items[0]["ipmi"]["value"] == opposite
            and mismatch_items[0]["redfish"]["value"] == base_power
            and mismatch_items[0]["ipmi"]["raw"]
            and mismatch_items[0]["redfish"]["raw"],
            {"inspection_id": mismatch_result["inspection_id"]},
        )
        http_json("DELETE", "http://127.0.0.1:9471/faults?device_id=bmc-03")

        http_json(
            "POST",
            "http://127.0.0.1:9471/faults",
            {
                "device_id": "bmc-01",
                "kind": "sensor_override",
                "sensor": "CPU Temp",
                "value": 99,
            },
        )
        service.run_once()
        first_violation_status = storage.latest_status(manager.config["rack_id"])
        first_violation = next(
            item
            for item in first_violation_status["active_violations"]
            if item["device_id"] == "bmc-01" and item["sensor_name"] == "CPU Temp"
        )
        time.sleep(max(0.1, args.high_round_delay))
        service.run_once()
        second_violation_status = storage.latest_status(manager.config["rack_id"])
        second_violation = next(
            item
            for item in second_violation_status["active_violations"]
            if item["device_id"] == "bmc-01" and item["sensor_name"] == "CPU Temp"
        )
        active_matches = [
            item
            for item in second_violation_status["active_violations"]
            if item["device_id"] == "bmc-01" and item["sensor_name"] == "CPU Temp"
        ]
        check(
            "continuous_violation_accumulates_one_active_record",
            first_violation["id"] == second_violation["id"]
            and len(active_matches) == 1
            and second_violation["duration_seconds"] >= int(args.high_round_delay)
            and second_violation["last_value"] == 99.0,
            {
                "violation_id": second_violation["id"],
                "duration_seconds": second_violation["duration_seconds"],
            },
        )

        http_json("DELETE", "http://127.0.0.1:9471/faults?device_id=bmc-01")
        time.sleep(0.2)
        service.run_once()
        recovered_status = storage.latest_status(manager.config["rack_id"])
        still_active = [
            item
            for item in recovered_status["active_violations"]
            if item["device_id"] == "bmc-01" and item["sensor_name"] == "CPU Temp"
        ]
        bmc01 = next(
            item for item in recovered_status["devices"] if item["device_id"] == "bmc-01"
        )
        resolution_count = storage.connection.execute(
            "SELECT COUNT(*) FROM violation_resolutions "
            "WHERE device_id='bmc-01' AND sensor_name='CPU Temp'"
        ).fetchone()[0]
        check(
            "recovery_creates_resolution_and_clears_active",
            not still_active
            and bmc01["violation_summary"]["resolved"] == 1
            and resolution_count == 1,
        )

        status_text = http_text("http://127.0.0.1:9470/status")
        status_payload = json.loads(status_text)
        canonical = json.dumps(
            status_payload, ensure_ascii=False, sort_keys=True, separators=(",", ":")
        )
        check(
            "status_json_is_stably_sorted_and_has_no_absolute_path",
            status_text == canonical and str(ROOT) not in status_text,
        )

        metrics_text = http_text("http://127.0.0.1:9470/metrics")
        metric_names = [
            "oob_collection_success_total",
            "oob_protocol_failures_total",
            "oob_fallback_collections_total",
            "oob_active_violations",
            "oob_unparsable_fields_total",
        ]
        check(
            "prometheus_metrics_stable_names_and_labels",
            all(name in metrics_text for name in metric_names)
            and all(f'device="{device_id}"' in metrics_text for device_id in (
                "bmc-01",
                "bmc-02",
                "bmc-03",
                "bmc-04",
                "bmc-05",
                "bmc-06",
            )),
        )

        series = json.loads(
            http_text(
                "http://127.0.0.1:9470/api/sensors"
                "?device_id=bmc-01&sensor=CPU%20Temp&limit=100"
            )
        )
        check(
            "sensor_series_query",
            series["count"] >= 3
            and all(item["device_id"] == "bmc-01" for item in series["observations"]),
            {"count": series["count"]},
        )

        old_generation = manager.generation
        old_thresholds = copy.deepcopy(manager.config["thresholds"])
        broken = copy.deepcopy(manager.config)
        profile_name = next(iter(broken["thresholds"]))
        broken["thresholds"][profile_name]["fan_rpm"]["lower"] = 9000
        broken["thresholds"][profile_name]["fan_rpm"]["upper"] = 100
        runtime_config_path.write_text(
            json.dumps(broken, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        reload_status, reload_body = http_json(
            "POST", "http://127.0.0.1:9470/admin/reload", {}
        )
        check(
            "invalid_threshold_reload_keeps_previous_config",
            reload_status == 409
            and reload_body["error"]["code"] == "E_THRESHOLD_INVALID"
            and manager.generation == old_generation
            and manager.config["thresholds"] == old_thresholds,
            {"error_code": reload_body["error"]["code"]},
        )
        runtime_config_path.write_text(
            json.dumps(config, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )

        report = {
            "generated_at": utc_now(),
            "rack": {
                "rack_id": manager.config["rack_id"],
                "seed": manager.config["seed"],
                "device_count": len(manager.config["devices"]),
                "firmware_profiles": sorted(manager.config["firmware_profiles"]),
            },
            "checks": checks,
            "baseline": {
                "inspection_id": baseline_result["inspection_id"],
                "conclusion": baseline["inspection"]["conclusion"],
                "devices": [
                    {
                        "device_id": item["device_id"],
                        "firmware_profile": item["firmware_profile"],
                        "status": item["status"],
                        "protocols_used": item["protocols_used"],
                        "power_state": item["power_state"],
                        "sensor_count": item["sensor_count"],
                    }
                    for item in baseline["devices"]
                ],
                "active_violations": baseline["active_violations"],
            },
            "fallback": {
                "device_id": fallback_device["device_id"],
                "primary_protocol": fallback_device["primary_protocol"],
                "protocols_used": fallback_device["protocols_used"],
                "fallback_used": fallback_device["fallback_used"],
                "redfish_error": redfish_result["error"],
            },
            "unparsable": {
                "device_id": "bmc-05",
                "fields": unparsable_items,
            },
            "inconsistency": mismatch_items,
            "violation_sequence": {
                "opened": first_violation,
                "ongoing": second_violation,
                "active_after_recovery": still_active,
                "resolutions": resolution_count,
            },
            "metrics": metrics_text.splitlines(),
            "status_snapshot": status_payload,
        }
    finally:
        try:
            monitor.stop()
        except Exception:
            pass
        control.stop()
        emulator.stop()
        storage.close()

    report["passed"] = all(item["passed"] for item in checks)
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(
        json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    print(
        json.dumps(
            {
                "passed": report["passed"],
                "checks": [
                    {"name": item["name"], "passed": item["passed"]} for item in checks
                ],
                "report": str(output),
            },
            ensure_ascii=False,
            indent=2,
            sort_keys=True,
        )
    )
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
