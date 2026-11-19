from __future__ import annotations

import copy
import json
import tempfile
import unittest
from pathlib import Path

from oob_rack.config import ConfigManager, validate_config
from oob_rack.errors import ThresholdConfigError
from oob_rack.generate import build_config
from oob_rack.ipmi import build_fru, ipmi_checksum, parse_fru
from oob_rack.normalization import (
    aggregate_device,
    compare_ipmi_redfish,
    normalize_protocol_payload,
)
from oob_rack.snmp import decode_oid, decode_integer, encode_integer, encode_oid


PROFILE = {"protocols": ["redfish", "ipmi", "snmp"], "power_state_shape": "object"}
THRESHOLDS = {
    "temperature_c": {"lower": 5.0, "upper": 70.0},
    "fan_rpm": {"lower": 600, "upper": 8000},
}


class ConfigTests(unittest.TestCase):
    def test_generated_rack_has_required_topology(self):
        config = build_config(1234)
        validate_config(config)
        self.assertEqual(len(config["devices"]), 6)
        self.assertEqual(len({d["firmware_profile"] for d in config["devices"]}), 3)
        for device in config["devices"]:
            self.assertEqual(len(device["power_supplies"]), 2)
            self.assertEqual(len(device["fans"]), 4)
        self.assertEqual(config["devices"][0]["ipmi_port"], 6230)
        self.assertEqual(config["devices"][-1]["ipmi_port"], 6235)
        self.assertEqual(config["devices"][0]["redfish_port"], 8443)
        self.assertEqual(config["devices"][-1]["snmp_port"], 1615)

    def test_invalid_threshold_reload_keeps_old_config(self):
        config = build_config(1234)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "rack.json"
            path.write_text(json.dumps(config), encoding="utf-8")
            manager = ConfigManager(path)
            old = copy.deepcopy(manager.config)
            broken = copy.deepcopy(config)
            profile = next(iter(broken["thresholds"]))
            broken["thresholds"][profile]["temperature_c"]["upper"] = 1.0
            path.write_text(json.dumps(broken), encoding="utf-8")
            with self.assertRaises(ThresholdConfigError) as raised:
                manager.reload()
            self.assertEqual(raised.exception.code, "E_THRESHOLD_INVALID")
            self.assertEqual(manager.config["thresholds"], old["thresholds"])
            self.assertEqual(manager.generation, 1)


class NormalizationTests(unittest.TestCase):
    def _redfish_payload(self, power_state):
        return {
            "asset": {
                "manufacturer": "Vendor",
                "model": "M",
                "serial": "S",
                "firmware_version": "new-3.0",
            },
            "power_raw": {
                "document": {
                    "PowerControl": [{"PowerState": power_state}],
                }
            },
            "sensors": [
                {
                    "name": "Inlet Temp",
                    "kind": "temperature",
                    "unit": "C",
                    "reading": 31,
                    "raw_entry": {"Name": "Inlet Temp", "ReadingCelsius": 31},
                }
            ],
            "raw": {"source": "unit-test"},
        }

    def test_object_and_string_power_shapes_normalize(self):
        object_result = normalize_protocol_payload(
            "redfish",
            self._redfish_payload({"State": "On", "Health": "OK"}),
            device_id="bmc-1",
            profile=PROFILE,
            thresholds=THRESHOLDS,
        )
        self.assertEqual(object_result["power"]["value"], "on")
        self.assertTrue(object_result["power"]["parsable"])
        string_profile = dict(PROFILE, power_state_shape="string")
        string_result = normalize_protocol_payload(
            "redfish",
            self._redfish_payload("Off"),
            device_id="bmc-1",
            profile=string_profile,
            thresholds=THRESHOLDS,
        )
        self.assertEqual(string_result["power"]["value"], "off")

    def test_unparsable_shape_is_not_power_loss(self):
        result = normalize_protocol_payload(
            "redfish",
            self._redfish_payload({"Unknown": {"Value": "On"}}),
            device_id="bmc-1",
            profile=PROFILE,
            thresholds=THRESHOLDS,
        )
        self.assertEqual(result["power"]["value"], "unknown")
        self.assertFalse(result["power"]["parsable"])
        self.assertEqual(len(result["unparsable"]), 1)
        self.assertIn("object PowerState", result["unparsable"][0]["reason"])
        self.assertTrue(result["usable"])

    def test_compare_requires_both_protocols(self):
        base = normalize_protocol_payload(
            "redfish",
            self._redfish_payload({"State": "On"}),
            device_id="bmc-1",
            profile=PROFILE,
            thresholds=THRESHOLDS,
        )
        self.assertEqual(compare_ipmi_redfish("bmc-1", {"redfish": base}), [])

    def test_compare_reports_values_and_raw_evidence(self):
        redfish = normalize_protocol_payload(
            "redfish",
            self._redfish_payload({"State": "On"}),
            device_id="bmc-1",
            profile=PROFILE,
            thresholds=THRESHOLDS,
        )
        ipmi = {
            "success": True,
            "usable": True,
            "power": {"value": "off", "parsable": True, "raw": '"off"'},
            "sensors": [
                {
                    "name": "Inlet Temp",
                    "kind": "temperature",
                    "value": 31.0,
                    "unit": "C",
                    "parsable": True,
                    "raw": "31",
                }
            ],
        }
        issues = compare_ipmi_redfish("bmc-1", {"ipmi": ipmi, "redfish": redfish})
        self.assertEqual(len(issues), 1)
        self.assertEqual(issues[0]["item"], "power_state")
        self.assertEqual(issues[0]["ipmi"]["raw"], '"off"')
        self.assertIn("On", issues[0]["redfish"]["raw"])

    def test_aggregate_old_firmware_does_not_report_inconsistency(self):
        ipmi = {
            "success": True,
            "usable": True,
            "asset": {"manufacturer": "V", "model": "M", "serial": "S", "firmware_version": "v1"},
            "power": {"value": "on", "parsable": True, "raw": "1"},
            "sensors": [],
            "unparsable": [],
        }
        device = {
            "device_id": "old",
            "rack_id": "rack",
            "firmware_profile": "legacy",
            "firmware_version": "v1",
        }
        profile = {"protocols": ["ipmi", "snmp"], "power_state_shape": "none"}
        result = aggregate_device(device=device, profile=profile, protocol_results={"ipmi": ipmi})
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["inconsistencies"], [])


class WireFormatTests(unittest.TestCase):
    def test_ipmi_checksum(self):
        self.assertEqual(sum(b"\x20\x06" + bytes((ipmi_checksum(b"\x20\x06"),))) & 0xFF, 0)

    def test_fru_round_trip(self):
        data = build_fru(
            manufacturer="Vendor",
            product="Model-1",
            serial="SERIAL-1",
            version="3.0",
        )
        parsed = parse_fru(data)
        self.assertEqual(parsed["manufacturer"], "Vendor")
        self.assertEqual(parsed["model"], "Model-1")
        self.assertEqual(parsed["serial"], "SERIAL-1")
        self.assertEqual(parsed["firmware_version"], "3.0")
        self.assertEqual(sum(data[:8]) & 0xFF, 0)
        self.assertEqual(sum(data[8 : data[3] * 8]) & 0xFF, 0)

    def test_snmp_ber_round_trip(self):
        oid = "1.3.6.1.4.1.41879.287.3.1.1.3.1"
        self.assertEqual(decode_oid(encode_oid(oid)), oid)
        for value in (0, 1, 127, 128, 41879):
            self.assertEqual(decode_integer(encode_integer(value)), value)


if __name__ == "__main__":
    unittest.main()
