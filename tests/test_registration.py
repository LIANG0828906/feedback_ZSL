from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from proto import training_pb2 as pb
from train.common import (
    expected_shard_assignments,
    load_config,
    load_manifest_for_config,
    resolve_path,
)
from train.coordinator import TrainingCoordinator


class RegistrationTests(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        config = load_config("config.json")
        config["database"] = str(Path(self.tempdir.name) / "state.sqlite3")
        self.config_path = Path(self.tempdir.name) / "config.json"
        self.config_path.write_text(json.dumps(config), encoding="utf-8")
        self.config = config
        self.manifest = load_manifest_for_config(config)
        self.coordinator = TrainingCoordinator(str(self.config_path))

    def tearDown(self):
        self.coordinator.close()
        self.tempdir.cleanup()

    def test_worker_duplicate_and_identity_conflict(self):
        request = pb.RegisterWorkerRequest(
            worker_id="worker-0",
            data_shard_index=0,
            sample_count=133334,
        )
        first = self.coordinator.register_worker(request)
        second = self.coordinator.register_worker(request)
        self.assertTrue(first.ok)
        self.assertTrue(second.ok)
        first_record = json.loads(first.record_json)
        second_record = json.loads(second.record_json)
        self.assertEqual(first_record["worker_id"], second_record["worker_id"])
        self.assertEqual(
            first_record["data_shard_index"], second_record["data_shard_index"]
        )
        self.assertEqual(first_record["sample_count"], second_record["sample_count"])
        conflict = self.coordinator.register_worker(
            pb.RegisterWorkerRequest(
                worker_id="worker-0",
                data_shard_index=1,
                sample_count=133333,
            )
        )
        self.assertFalse(conflict.ok)
        self.assertEqual(conflict.code, "WORKER_IDENTITY_CONFLICT")
        unchanged = json.loads(conflict.record_json)
        self.assertEqual(unchanged["data_shard_index"], 0)
        self.assertEqual(unchanged["sample_count"], 133334)

    def test_shard_duplicate_and_parameter_set_conflict(self):
        assignments = expected_shard_assignments(
            self.manifest, [item["shard_id"] for item in self.config["shards"]]
        )
        request = pb.RegisterShardRequest(
            shard_id="shard-0",
            endpoint="127.0.0.1:59001",
            parameter_names=assignments["shard-0"],
        )
        first = self.coordinator.register_shard(request)
        second = self.coordinator.register_shard(request)
        self.assertTrue(first.ok)
        self.assertTrue(second.ok)
        first_record = json.loads(first.record_json)
        second_record = json.loads(second.record_json)
        self.assertEqual(first_record["shard_id"], second_record["shard_id"])
        self.assertEqual(
            first_record["declared_parameter_names"],
            second_record["declared_parameter_names"],
        )
        conflict = self.coordinator.register_shard(
            pb.RegisterShardRequest(
                shard_id="shard-0",
                endpoint="127.0.0.1:59002",
                parameter_names=["weight.000.00"],
            )
        )
        self.assertFalse(conflict.ok)
        self.assertEqual(conflict.code, "SHARD_PARAMETER_SET_CONFLICT")
        unchanged = json.loads(conflict.record_json)
        self.assertEqual(
            unchanged["declared_parameter_names"], sorted(assignments["shard-0"])
        )
        owned = self.coordinator.connection.execute(
            "SELECT COUNT(*) AS count FROM shard_ownership WHERE shard_id = 'shard-0'"
        ).fetchone()["count"]
        self.assertEqual(owned, len(assignments["shard-0"]))


if __name__ == "__main__":
    unittest.main()
