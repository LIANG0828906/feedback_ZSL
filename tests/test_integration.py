from __future__ import annotations

import copy
import base64
import json
import socket
import ssl
import tempfile
import unittest
import urllib.request
from datetime import datetime, timedelta, timezone
from pathlib import Path

from oob_rack.collector import RackCollector
from oob_rack.config import ConfigManager, load_config, resolve_path
from oob_rack.emulator import BmcEmulator
from oob_rack.emulator_state import FaultController
from oob_rack.generate import build_config
from oob_rack.storage import RackStorage, utc_now


def _aggregate(device_id: str, value: float, rack: str = "rack-a") -> dict:
    sensor = {
        "name": "CPU Temp",
        "kind": "temperature",
        "value": value,
        "unit": "C",
        "lower": 5.0,
        "upper": 70.0,
        "parsable": True,
        "raw": json.dumps(value),
        "source_protocol": "ipmi",
    }
    return {
        "device_id": device_id,
        "rack_id": rack,
        "firmware_profile": "mid-2.4",
        "firmware_version": "mid-2.4",
        "status": "ok",
        "primary_protocol": "ipmi",
        "protocols_used": ["ipmi"],
        "fallback_used": False,
        "asset": {
            "manufacturer": "Vendor",
            "model": "Model",
            "serial": "Serial",
            "firmware_version": "mid-2.4",
        },
        "power": {
            "value": "on",
            "parsable": True,
            "source_protocol": "ipmi",
            "raw": "1",
        },
        "sensors": [sensor],
        "unparsable_count": 0,
        "unparsable": [],
        "inconsistencies": [],
        "protocols": {
            "ipmi": {
                "success": True,
                "usable": True,
                "error": "",
                "duration_ms": 1,
                "raw_response": {"test": True},
                "unparsable": [],
            }
        },
        "duration_ms": 1,
    }


class StorageTests(unittest.TestCase):
    def test_violation_duration_resolution_and_series(self):
        with tempfile.TemporaryDirectory() as directory:
            storage = RackStorage(Path(directory) / "test.db")
            try:
                first = storage.start_or_resume_inspection(
                    rack_id="rack-a",
                    fingerprint="fingerprint",
                    config_version=1,
                    expected_devices=1,
                )
                t1 = (datetime.now(timezone.utc) - timedelta(seconds=10)).isoformat(
                    timespec="milliseconds"
                ).replace("+00:00", "Z")
                storage.record_device_result(
                    first["inspection_id"], _aggregate("bmc-01", 99.0), observed_at=t1
                )
                storage.complete_inspection(first["inspection_id"])

                second = storage.start_or_resume_inspection(
                    rack_id="rack-a",
                    fingerprint="fingerprint",
                    config_version=1,
                    expected_devices=1,
                )
                self.assertNotEqual(first["inspection_id"], second["inspection_id"])
                t2 = utc_now()
                storage.record_device_result(
                    second["inspection_id"], _aggregate("bmc-01", 99.0), observed_at=t2
                )
                storage.complete_inspection(second["inspection_id"])
                status = storage.latest_status("rack-a")
                self.assertEqual(len(status["active_violations"]), 1)
                self.assertGreaterEqual(status["active_violations"][0]["duration_seconds"], 9)

                third = storage.start_or_resume_inspection(
                    rack_id="rack-a",
                    fingerprint="fingerprint",
                    config_version=1,
                    expected_devices=1,
                )
                storage.record_device_result(
                    third["inspection_id"], _aggregate("bmc-01", 50.0), observed_at=utc_now()
                )
                storage.complete_inspection(third["inspection_id"])
                status = storage.latest_status("rack-a")
                self.assertEqual(status["active_violations"], [])
                self.assertEqual(status["devices"][0]["violation_summary"]["resolved"], 1)
                row = storage.connection.execute(
                    "SELECT status, resolved_at FROM violations WHERE id=1"
                ).fetchone()
                self.assertEqual(row["status"], "resolved")
                self.assertIsNotNone(row["resolved_at"])
                resolutions = storage.connection.execute(
                    "SELECT COUNT(*) FROM violation_resolutions"
                ).fetchone()[0]
                self.assertEqual(resolutions, 1)
                series = storage.sensor_series(device_id="bmc-01", sensor_name="CPU Temp")
                self.assertEqual(len(series), 3)
            finally:
                storage.close()


class EmulatorIntegrationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.config = copy.deepcopy(load_config("config/rack.json"))
        cls.config["devices"] = copy.deepcopy(cls.config["devices"])
        cls.faults = FaultController()
        cls.emulator = BmcEmulator(cls.config, cls.faults)
        cls.emulator.start()
        cls.collector = RackCollector(cls.config)

    @classmethod
    def tearDownClass(cls):
        cls.emulator.stop()

    def test_mixed_firmware_and_deterministic_readings(self):
        first = self.collector.collect_rack()
        second = self.collector.collect_rack()
        self.assertEqual(len(first["devices"]), 6)
        by_id = {item["device_id"]: item for item in first["devices"]}
        self.assertEqual(by_id["bmc-01"]["protocols_used"], ["ipmi", "snmp"])
        self.assertNotIn("redfish", by_id["bmc-01"]["protocols_used"])
        self.assertIn("redfish", by_id["bmc-02"]["protocols_used"])
        self.assertEqual(by_id["bmc-03"]["power"]["value"], "off")
        for left, right in zip(first["devices"], second["devices"]):
            for sensor in left["sensors"]:
                peers = {item["name"]: item for item in right["sensors"]}
                self.assertEqual(sensor["value"], peers[sensor["name"]]["value"])
        self.assertTrue(all(not item["inconsistencies"] for item in first["devices"]))

    def test_redfish_failure_falls_back_without_unsupported_flag(self):
        fault = self.faults.add(
            {"device_id": "bmc-02", "kind": "offline", "protocol": "redfish"}
        )
        try:
            result = self.collector.collect_device(self.config["devices"][1])
        finally:
            self.faults.clear(fault_id=fault["id"])
        self.assertEqual(result["primary_protocol"], "ipmi")
        self.assertTrue(result["fallback_used"])
        self.assertFalse(result["protocols"]["redfish"]["success"])
        self.assertNotIn("unsupported", result["protocols"]["redfish"])
        self.assertEqual(result["status"], "degraded")

    def test_unparsable_redfish_field_is_not_offline(self):
        fault = self.faults.add(
            {"device_id": "bmc-05", "kind": "redfish_power_unparsable"}
        )
        try:
            result = self.collector.collect_device(self.config["devices"][4])
        finally:
            self.faults.clear(fault_id=fault["id"])
        self.assertGreaterEqual(result["unparsable_count"], 1)
        self.assertIn(result["status"], ("degraded", "ok"))
        self.assertNotEqual(result["status"], "failed")
        self.assertTrue(
            any(item["device_id"] == "bmc-05" for item in result["unparsable"])
        )

    def test_cross_protocol_mismatch_contains_both_raw_values(self):
        fault = self.faults.add(
            {
                "device_id": "bmc-03",
                "kind": "power_override",
                "protocol": "ipmi",
                "value": "on",
            }
        )
        try:
            result = self.collector.collect_device(self.config["devices"][2])
        finally:
            self.faults.clear(fault_id=fault["id"])
        issues = [item for item in result["inconsistencies"] if item["item"] == "power_state"]
        self.assertEqual(len(issues), 1)
        self.assertEqual(issues[0]["ipmi"]["value"], "on")
        self.assertEqual(issues[0]["redfish"]["value"], "off")
        self.assertTrue(issues[0]["ipmi"]["raw"])
        self.assertTrue(issues[0]["redfish"]["raw"])

    def test_redfish_firmware_shapes_and_legacy_port(self):
        context = ssl.create_default_context(
            cafile=str(resolve_path(self.config, self.config["redfish"]["ca_file"]))
        )
        credential = base64.b64encode(b"admin:admin123").decode("ascii")

        def power(device_index: int) -> dict:
            request = urllib.request.Request(
                f"https://127.0.0.1:{self.config['devices'][device_index]['redfish_port']}"
                "/redfish/v1/Chassis/1/Power"
            )
            request.add_header("Authorization", f"Basic {credential}")
            with urllib.request.urlopen(request, context=context, timeout=3) as response:
                return json.load(response)

        middle = power(1)
        new = power(2)
        self.assertIsInstance(middle["PowerControl"][0]["PowerState"], str)
        self.assertNotIn("Actions", middle)
        self.assertIsInstance(new["PowerControl"][0]["PowerState"], dict)
        self.assertIn("#Chassis.Reset", new["Actions"])

        legacy_probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        try:
            legacy_probe.settimeout(0.5)
            self.assertNotEqual(
                legacy_probe.connect_ex(
                    ("127.0.0.1", self.config["devices"][0]["redfish_port"])
                ),
                0,
            )
        finally:
            legacy_probe.close()


if __name__ == "__main__":
    unittest.main()
