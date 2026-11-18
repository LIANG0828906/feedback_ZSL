from __future__ import annotations

import shutil
import tempfile
import unittest
from pathlib import Path

import torch

from mlp_ps.checkpoint import capture_snapshot, validate_checkpoint, write_checkpoint
from mlp_ps.config import load_config
from mlp_ps.errors import BusinessError, E_CKPT_CORRUPT, E_CKPT_MISSING
from mlp_ps.layout import build_layout
from mlp_ps.model import build_model, build_optimizer, parameter_shapes


class CheckpointTests(unittest.TestCase):
    def _make_snapshot(self):
        config = load_config(Path(__file__).resolve().parents[1] / "config" / "demo.json")
        model = build_model(config.seed, config.input_dim, config.hidden_dim, config.output_dim)
        optimizer = build_optimizer(model, config.learning_rate, config.momentum)
        layout = build_layout(parameter_shapes(model))
        consumed = {shard_id: 0 for shard_id in range(4)}
        return config, capture_snapshot(config, layout, model, optimizer, consumed, 0)

    def test_checkpoint_round_trip_and_corruption_detection(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config, snapshot = self._make_snapshot()
            event = write_checkpoint(snapshot, root, 20_000_000.0, 4096)
            self.assertEqual(event["status"], "OK")
            info = validate_checkpoint(Path(event["path"]))
            self.assertEqual(info.global_step, 0)
            shard_file = Path(event["path"]) / "shard-1.bin"
            data = bytearray(shard_file.read_bytes())
            data[0] ^= 0xFF
            shard_file.write_bytes(data)
            with self.assertRaises(BusinessError) as captured:
                validate_checkpoint(Path(event["path"]))
            self.assertEqual(captured.exception.code, E_CKPT_CORRUPT)

    def test_missing_checkpoint_has_distinct_code(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            with self.assertRaises(BusinessError) as captured:
                validate_checkpoint(Path(temporary) / "does-not-exist")
            self.assertEqual(captured.exception.code, E_CKPT_MISSING)


if __name__ == "__main__":
    unittest.main()
