from __future__ import annotations

import unittest

from mlp_ps.errors import BusinessError, E_LAYOUT_CONFLICT
from mlp_ps.layout import build_layout, validate_layout_spec


class LayoutTests(unittest.TestCase):
    def test_layout_is_complete_and_non_overlapping(self) -> None:
        layout = build_layout(
            [
                ("fc1.weight", (32, 16)),
                ("fc1.bias", (32,)),
                ("fc2.weight", (4, 32)),
                ("fc2.bias", (4,)),
            ]
        )
        validate_layout_spec(layout.spec)
        for parameter in layout.spec["parameters"]:
            covered = set()
            for slice_info in parameter["slices"]:
                start = slice_info["start"]
                end = start + slice_info["length"]
                values = set(range(start, end))
                self.assertFalse(covered & values)
                covered |= values
            self.assertEqual(covered, set(range(parameter["shape"][0])))

    def test_duplicate_slice_is_rejected(self) -> None:
        layout = build_layout([("weight", (8, 4))])
        broken = {
            "format": "mlp-ps-layout-v1",
            "shard_count": 4,
            "parameters": [
                {
                    "name": "weight",
                    "shape": [8, 4],
                    "slices": [
                        {"shard_id": shard_id, "start": 0, "length": 2, "shape": [2, 4]}
                        for shard_id in range(4)
                    ],
                }
            ],
        }
        with self.assertRaises(BusinessError) as captured:
            validate_layout_spec(broken)
        self.assertEqual(captured.exception.code, E_LAYOUT_CONFLICT)


if __name__ == "__main__":
    unittest.main()
