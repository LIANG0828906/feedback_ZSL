from __future__ import annotations

from .deterministic import seeded_rng

FIRMWARE_PROFILES = {
    "legacy-1.2": {
        "label": "legacy",
        "protocols": ["ipmi", "snmp"],
        "power_state_shape": "none",
    },
    "mid-2.4": {
        "label": "middle",
        "protocols": ["redfish", "ipmi", "snmp"],
        "power_state_shape": "string",
    },
    "new-3.0": {
        "label": "new",
        "protocols": ["redfish", "ipmi", "snmp"],
        "power_state_shape": "object",
    },
}

THRESHOLDS = {
    "legacy-1.2": {
        "temperature_c": {"lower": 5.0, "upper": 70.0},
        "fan_rpm": {"lower": 650, "upper": 8000},
    },
    "mid-2.4": {
        "temperature_c": {"lower": 5.0, "upper": 66.0},
        "fan_rpm": {"lower": 800, "upper": 8000},
    },
    "new-3.0": {
        "temperature_c": {"lower": 5.0, "upper": 64.0},
        "fan_rpm": {"lower": 850, "upper": 8000},
    },
}

PROFILE_SEQUENCE = ("legacy-1.2", "mid-2.4", "new-3.0", "legacy-1.2", "mid-2.4", "new-3.0")


def build_config(seed: int = 2872026) -> dict:
    devices = []
    for index, firmware_profile in enumerate(PROFILE_SEQUENCE, start=1):
        device_id = f"bmc-{index:02d}"
        rng = seeded_rng(seed, device_id, "asset")
        serial = f"SN-{seed % 100000:05d}-{rng.randrange(100000, 999999)}"
        profile = FIRMWARE_PROFILES[firmware_profile]
        devices.append(
            {
                "device_id": device_id,
                "rack_id": "rack-a",
                "slot": index,
                "manufacturer": "OpenRack Labs",
                "model": f"OR-2U-{rng.choice(['A1', 'B2', 'C3'])}",
                "serial": serial,
                "firmware_version": firmware_profile,
                "firmware_profile": firmware_profile,
                "ipmi_port": 6230 + index - 1,
                "redfish_port": 8443 + index - 1,
                "snmp_port": 1610 + index - 1,
                "fallback_protocols": list(profile["protocols"]),
                "power_supplies": ["PSU 1", "PSU 2"],
                "fans": ["Fan 1", "Fan 2", "Fan 3", "Fan 4"],
            }
        )
    return {
        "version": 1,
        "rack_id": "rack-a",
        "seed": seed,
        "listen_host": "127.0.0.1",
        "devices": devices,
        "firmware_profiles": FIRMWARE_PROFILES,
        "thresholds": THRESHOLDS,
        "inspection": {
            "interval_seconds": 5.0,
            "timeout_seconds": 0.8,
            "retries": 1,
            "workers": 2,
            "database": "../artifacts/inspections.db",
            "start_immediately": True,
        },
        "ipmi": {
            "username": "admin",
            "password": "admin123",
            "cipher_suite": 1,
        },
        "redfish": {
            "username": "admin",
            "password": "admin123",
            "verify_tls": True,
            "ca_file": "../certs/bmc-cert.pem",
            "server_cert": "../certs/bmc-cert.pem",
            "server_key": "../certs/bmc-key.pem",
        },
        "snmp": {
            "community": "public",
            "enterprise_base": "1.3.6.1.4.1.41879.287",
        },
        "monitor": {"host": "127.0.0.1", "port": 9470},
        "control": {"host": "127.0.0.1", "port": 9471},
    }
