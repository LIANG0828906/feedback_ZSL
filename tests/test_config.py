from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest

from app.config import ConfigError, load_config, parse_config


def valid_tenant(**overrides: object) -> dict[str, object]:
    value: dict[str, object] = {
        "tenant_id": "tenant-a",
        "priority": "high",
        "max_concurrency": 2,
        "max_retries": 1,
        "backoff_base_ms": 10,
        "timeout_ms": 500,
        "degrade_after_timeouts": 2,
        "receiver_url": "http://receiver-a:8080",
    }
    value.update(overrides)
    return value


class ConfigTests(unittest.TestCase):
    def assert_code(self, code: str, tenant: dict[str, object]) -> None:
        with self.assertRaises(ConfigError) as caught:
            parse_config({"tenants": [tenant]})
        self.assertEqual(code, caught.exception.code)

    def test_valid_configuration(self) -> None:
        tenants = parse_config({"tenants": [valid_tenant()]})
        self.assertEqual("tenant-a", tenants[0].tenant_id)
        self.assertEqual(0, tenants[0].priority_rank)

    def test_all_invalid_business_codes(self) -> None:
        cases = {
            "CFG_PRIORITY_INVALID": valid_tenant(priority="critical"),
            "CFG_CONCURRENCY_INVALID": valid_tenant(max_concurrency=0),
            "CFG_RETRIES_INVALID": valid_tenant(max_retries=-1),
            "CFG_BACKOFF_INVALID": valid_tenant(backoff_base_ms=-1),
            "CFG_TIMEOUT_INVALID": valid_tenant(timeout_ms=0),
            "CFG_DEGRADE_THRESHOLD_INVALID": valid_tenant(
                degrade_after_timeouts=-1
            ),
            "CFG_RECEIVER_URL_INVALID": valid_tenant(receiver_url="not-a-url"),
            "CFG_TENANT_ID_INVALID": valid_tenant(tenant_id="Tenant A"),
        }
        for code, tenant in cases.items():
            with self.subTest(code=code):
                self.assert_code(code, tenant)

    def test_duplicate_tenant_is_rejected(self) -> None:
        with self.assertRaises(ConfigError) as caught:
            parse_config(
                {"tenants": [valid_tenant(), valid_tenant(priority="low")]}
            )
        self.assertEqual("CFG_DUPLICATE_TENANT", caught.exception.code)

    def test_invalid_configuration_exits_without_creating_database(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            database_path = os.path.join(directory, "should-not-exist.db")
            environment = dict(os.environ)
            environment["TENANT_CONFIG_JSON"] = json.dumps(
                {"tenants": [valid_tenant(priority="urgent")]}
            )
            environment["DB_PATH"] = database_path
            completed = subprocess.run(
                [sys.executable, "-m", "app.main", "--check-config"],
                cwd=os.path.dirname(os.path.dirname(__file__)),
                env=environment,
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(2, completed.returncode)
            self.assertIn("CFG_PRIORITY_INVALID", completed.stderr)
            self.assertFalse(os.path.exists(database_path))

    def test_missing_file_has_stable_code(self) -> None:
        old = os.environ.pop("TENANT_CONFIG_JSON", None)
        try:
            with self.assertRaises(ConfigError) as caught:
                load_config("/definitely/missing/tenants.json")
            self.assertEqual("CFG_FILE_MISSING", caught.exception.code)
        finally:
            if old is not None:
                os.environ["TENANT_CONFIG_JSON"] = old


if __name__ == "__main__":
    unittest.main()

