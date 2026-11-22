from __future__ import annotations

import tempfile
import unittest

from tests.helpers import sample_path
from vp_pipeline.checkpoint import CheckpointStore
from vp_pipeline.errors import E_PARA, PipelineError
from vp_pipeline.loader import load_scene
from vp_pipeline.renderer import render_all_frames, render_frame
from vp_pipeline.session import CompositorSession


class CompositorTests(unittest.TestCase):
    def test_repeated_composition_is_pixel_identical(self) -> None:
        model = load_scene(sample_path(3))
        first = render_all_frames(model)
        second = render_all_frames(model)
        self.assertEqual(
            {index: frame.content_hash for index, frame in first.items()},
            {index: frame.content_hash for index, frame in second.items()},
        )
        for index in first:
            self.assertEqual(first[index].pixel_bytes, second[index].pixel_bytes)

    def test_frame_metadata_is_derived_from_model(self) -> None:
        model = load_scene(sample_path(2))
        frame = render_frame(model, 7)
        self.assertEqual(frame.width, model.canvas.width)
        self.assertEqual(frame.height, model.canvas.height)
        self.assertEqual(frame.channels, model.canvas.channels)
        self.assertEqual(
            (frame.fps_num, frame.fps_den),
            (model.timeline.fps_num, model.timeline.fps_den),
        )
        self.assertGreater(frame.mean_alpha, 0.0)
        self.assertGreater(frame.mean_linear_luminance, 0.0)

    def test_frame_index_out_of_range_returns_e_para(self) -> None:
        model = load_scene(sample_path(1))
        with self.assertRaises(PipelineError) as caught:
            render_frame(model, 18)
        self.assertEqual(caught.exception.code, E_PARA)
        self.assertEqual(caught.exception.field_path, "frame_index")

    def test_layer_parameter_out_of_range_returns_e_para(self) -> None:
        model = load_scene(sample_path(1))
        with tempfile.TemporaryDirectory() as directory:
            session = CompositorSession(model, CheckpointStore(directory))
            with self.assertRaises(PipelineError) as caught:
                session.apply_edit("character", "opacity", 1.25)
            self.assertEqual(caught.exception.code, E_PARA)
            self.assertEqual(caught.exception.field_path, "opacity")
        with self.assertRaises(PipelineError) as caught:
            render_frame(model, -1)
        self.assertEqual(caught.exception.code, E_PARA)


if __name__ == "__main__":
    unittest.main()
