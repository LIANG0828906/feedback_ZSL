from __future__ import annotations

import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from ordersync.config import ServiceConfig, load_config
from ordersync.errors import TopologyError

from .helpers import base_config_dict


class ConfigTests(unittest.TestCase):
    def test_environment_substitution_is_all_or_nothing(self) -> None:
        raw = base_config_dict()
        raw["primary"]["connection_string"] = "pwd=${MISSING_ORDER_PASSWORD}"
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.json"
            path.write_text(json.dumps(raw), encoding="utf-8")
            with mock.patch.dict(os.environ, {}, clear=True):
                with self.assertRaises(TopologyError) as raised:
                    load_config(path)
        self.assertEqual(raised.exception.code, "E_TOPOLOGY")
        self.assertEqual(raised.exception.details["reason"], "config_env_missing")

    def test_same_endpoint_is_rejected(self) -> None:
        raw = base_config_dict()
        raw["replica"] = dict(raw["primary"])
        with self.assertRaises(TopologyError) as raised:
            ServiceConfig.parse(raw)
        self.assertEqual(raised.exception.code, "E_TOPOLOGY")
        self.assertEqual(raised.exception.details["reason"], "same_endpoint")

    def test_missing_replication_mode_is_rejected(self) -> None:
        raw = base_config_dict()
        del raw["replication"]["mode"]
        with self.assertRaises(TopologyError) as raised:
            ServiceConfig.parse(raw)
        self.assertEqual(raised.exception.code, "E_TOPOLOGY")


if __name__ == "__main__":
    unittest.main()

