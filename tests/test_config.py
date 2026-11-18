from __future__ import annotations

import copy
import json
import tempfile
import unittest
from pathlib import Path

from inference_router.config import ConfigError, load_router_config
from inference_router.modelgen import generate_models


class ConfigValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        generate_models(self.root / "models")
        self.base = {
            "schema_version": 1,
            "service": {
                "router_grpc": "127.0.0.1:19080",
                "http": "127.0.0.1:19081",
                "db_path": "router.sqlite3",
                "request_timeout_ms": 120,
                "l2_attempts": 2,
                "thread_pool_size": 8,
            },
            "health": {
                "window_size": 32,
                "min_samples": 4,
                "probe_interval_ms": 500,
                "probe_timeout_ms": 100,
                "probe_failures_to_evict": 2,
                "probe_successes_to_recover": 2,
            },
            "stats": {"window_size": 100, "p99_threshold_ms": 120},
            "layers": [
                {
                    "layer_id": "l1",
                    "name": "embedding",
                    "p95_threshold_ms": 25,
                    "call_timeout_ms": 40,
                    "depends_on": [],
                },
                {
                    "layer_id": "l2",
                    "name": "ranking",
                    "p95_threshold_ms": 70,
                    "call_timeout_ms": 50,
                    "depends_on": ["l1"],
                },
            ],
            "shards": [
                self._shard("l1", "embedding", "l1-a", "l1-a.onnx", 19101),
                self._shard("l2", "ranking", "l2-a", "l2-a.onnx", 19102),
            ],
        }

    def tearDown(self) -> None:
        self.temp.cleanup()

    @staticmethod
    def _shard(layer: str, group: str, replica_id: str, model: str, port: int) -> dict:
        return {
            "layer": layer,
            "replica_group": group,
            "replica_id": replica_id,
            "model_path": f"models/{model}",
            "listen_address": f"127.0.0.1:{port}",
            "input_dim": 64,
            "output_dim": 64,
        }

    def _write(self, data: dict) -> Path:
        path = self.root / "router.json"
        path.write_text(json.dumps(data), encoding="utf-8")
        return path

    def _assert_error(
        self,
        data: dict,
        code: str,
        replica_id: str | None = None,
        field: str | None = None,
    ) -> None:
        with self.assertRaises(ConfigError) as caught:
            load_router_config(self._write(data))
        self.assertEqual(caught.exception.error_code, code)
        if replica_id is not None:
            self.assertEqual(caught.exception.replica_id, replica_id)
        if field is not None:
            self.assertEqual(caught.exception.field_name, field)

    def test_valid_config(self) -> None:
        config = load_router_config(self._write(self.base))
        self.assertEqual([layer.layer_id for layer in config.layers], ["l1", "l2"])
        self.assertEqual(len(config.shards), 2)

    def test_duplicate_replica_id(self) -> None:
        data = copy.deepcopy(self.base)
        data["shards"][1]["replica_id"] = "l1-a"
        self._assert_error(
            data,
            "CONFIG_DUPLICATE_REPLICA",
            replica_id="l1-a",
            field="shards[1].replica_id",
        )

    def test_duplicate_replica_group_across_layers(self) -> None:
        data = copy.deepcopy(self.base)
        data["shards"][1]["replica_group"] = "embedding"
        self._assert_error(
            data,
            "CONFIG_DUPLICATE_REPLICA_GROUP",
            replica_id="l2-a",
            field="shards[1].replica_group",
        )

    def test_layer_cycle(self) -> None:
        data = copy.deepcopy(self.base)
        data["layers"][0]["depends_on"] = ["l2"]
        self._assert_error(data, "CONFIG_LAYER_CYCLE", field="layers.depends_on")

    def test_missing_model(self) -> None:
        data = copy.deepcopy(self.base)
        data["shards"][1]["model_path"] = "models/not-there.onnx"
        self._assert_error(
            data,
            "CONFIG_MODEL_MISSING",
            replica_id="l2-a",
            field="model_path",
        )

    def test_model_dimension_mismatch(self) -> None:
        data = copy.deepcopy(self.base)
        data["shards"][0]["input_dim"] = 32
        self._assert_error(
            data,
            "CONFIG_DIMENSION_MISMATCH",
            replica_id="l1-a",
            field="input_dim",
        )

    def test_pipeline_dimension_mismatch(self) -> None:
        data = copy.deepcopy(self.base)
        data["shards"][1]["output_dim"] = 32
        with self.assertRaises(ConfigError) as caught:
            load_router_config(self._write(data))
        self.assertEqual(caught.exception.error_code, "CONFIG_DIMENSION_MISMATCH")
        self.assertEqual(caught.exception.replica_id, "l2-a")


if __name__ == "__main__":
    unittest.main()
