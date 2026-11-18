from __future__ import annotations

import unittest

from mlp_ps.config import Config, validate_config
from mlp_ps.errors import BusinessError, E_PROGRESS_RANGE
from pathlib import Path


class ConfigTests(unittest.TestCase):
    def test_sample_budget_out_of_range(self) -> None:
        config = Config(
            raw={
                "run": {"seed": 1, "host": "127.0.0.1", "port": 29617},
                "model": {"input_dim": 16, "hidden_dim": 32, "output_dim": 4},
                "training": {
                    "sample_count": 10,
                    "batch_size": 8,
                    "total_steps": 2,
                    "learning_rate": 0.01,
                    "momentum": 0.9,
                },
                "checkpoint": {"steps": [0], "bandwidth_mbps": 20.0},
            },
            source_path=Path("inline"),
            run_dir=Path("artifacts/test"),
        )
        with self.assertRaises(BusinessError) as captured:
            validate_config(config)
        self.assertEqual(captured.exception.code, E_PROGRESS_RANGE)


if __name__ == "__main__":
    unittest.main()
