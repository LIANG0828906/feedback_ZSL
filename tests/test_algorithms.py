from __future__ import annotations

import unittest

import numpy as np

from edgeforge.algorithms import apply_stage, gradient_magnitude, prune_short_components
from edgeforge.config import PipelineConfig
from edgeforge.errors import ParameterError, StateError
from edgeforge.stages import BlockState, BlockStateMachine, Stage


class AlgorithmTests(unittest.TestCase):
    def test_parameter_validation_errors(self) -> None:
        cases = [
            (PipelineConfig(low_threshold=20, high_threshold=10), "low_threshold"),
            (PipelineConfig(open_size=4), "open_size"),
            (PipelineConfig(close_size=0), "close_size"),
            (PipelineConfig(thin_iterations=0), "thin_iterations"),
            (PipelineConfig(prune_length=0), "prune_length"),
            (PipelineConfig(gradient_operator="unknown"), "gradient_operator"),
            (PipelineConfig(tile_size=2), "tile_size"),
            (PipelineConfig(band_override=-1), "band_override"),
        ]
        for config, field in cases:
            with self.subTest(field=field):
                with self.assertRaises(ParameterError) as context:
                    config.validate()
                self.assertEqual(context.exception.code, "E_PARA")
                self.assertEqual(context.exception.field, field)

    def test_gradient_channel_shape_and_range(self) -> None:
        rng = np.random.default_rng(7)
        gray = rng.integers(0, 256, size=(40, 50), dtype=np.uint8)
        rgb = rng.integers(0, 256, size=(40, 50, 3), dtype=np.uint8)
        gray_mag = gradient_magnitude(gray, "sobel")
        rgb_mag = gradient_magnitude(rgb, "prewitt")
        self.assertEqual(gray_mag.shape, gray.shape)
        self.assertEqual(rgb_mag.shape, rgb.shape)
        self.assertEqual(gray_mag.dtype, np.float32)
        self.assertGreaterEqual(float(gray_mag.min()), 0.0)
        self.assertLessEqual(float(gray_mag.max()), 255.0)
        self.assertTrue(np.array_equal(gray_mag, gradient_magnitude(gray, "sobel")))

    def test_state_machine_legal_and_illegal_transitions(self) -> None:
        machine = BlockStateMachine((0, 1))
        cell = machine.transition(Stage.GRADIENT, 0, BlockState.PROCESSING)
        self.assertEqual(cell.attempts, 1)
        machine.transition(Stage.GRADIENT, 0, BlockState.FAILED, error="boom")
        self.assertEqual(machine.cell(Stage.GRADIENT, 0).state, BlockState.FAILED)
        with self.assertRaises(StateError) as context:
            machine.transition(Stage.GRADIENT, 0, BlockState.COMMITTED)
        self.assertEqual(context.exception.code, "E_STATE")
        self.assertEqual(
            machine.cell(Stage.GRADIENT, 0).state,
            BlockState.FAILED,
        )
        self.assertEqual(machine.committed, 0)

    def test_band_linked_stage_consistency(self) -> None:
        rng = np.random.default_rng(21)
        height, width = 96, 112
        mask = (rng.random((height, width)) < 0.08).astype(np.uint8)
        mask[:8, :20] = 1
        mask[70:72, 20:70] = 1
        config_cases = [
            (Stage.OPEN, PipelineConfig(open_size=7)),
            (Stage.CLOSE, PipelineConfig(close_size=7)),
            (Stage.THIN, PipelineConfig(thin_iterations=6)),
            (Stage.PRUNE, PipelineConfig(prune_length=5)),
        ]
        tile = 32
        for stage, config in config_cases:
            with self.subTest(stage=stage.value):
                whole = apply_stage(stage, mask, config)
                assembled = np.zeros_like(whole)
                band = config.required_band(stage)
                for y0 in range(0, height, tile):
                    for x0 in range(0, width, tile):
                        y1 = min(height, y0 + tile)
                        x1 = min(width, x0 + tile)
                        ey0 = max(0, y0 - band)
                        ey1 = min(height, y1 + band)
                        ex0 = max(0, x0 - band)
                        ex1 = min(width, x1 + band)
                        protected = (
                            ey0 > 0,
                            ey1 < height,
                            ex0 > 0,
                            ex1 < width,
                        )
                        processed = apply_stage(
                            stage,
                            mask[ey0:ey1, ex0:ex1],
                            config,
                            protected_borders=protected,
                        )
                        assembled[y0:y1, x0:x1] = processed[
                            y0 - ey0 : y1 - ey0,
                            x0 - ex0 : x1 - ex0,
                        ]
                self.assertTrue(
                    np.array_equal(whole, assembled),
                    f"{stage.value} band={band} differs from whole-image result",
                )

    def test_prune_removes_short_component_only(self) -> None:
        mask = np.zeros((30, 40), dtype=np.uint8)
        mask[5:7, 5:7] = 1
        mask[15:17, 10:30] = 1
        pruned = prune_short_components(mask, 5)
        self.assertEqual(int(pruned[5:7, 5:7].sum()), 0)
        self.assertEqual(int(pruned[15:17, 10:30].sum()), 40)


if __name__ == "__main__":
    unittest.main()

