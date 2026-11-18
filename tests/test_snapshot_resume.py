import json
import os
import shutil
import tempfile
import unittest

from kiln_glass.compare import compare_runs
from kiln_glass.io_utils import read_json
from kiln_glass.render import GracefulInterrupt, Renderer
from kiln_glass.scene import build_default_scene, canonical_scene_bytes, parse_scene
from kiln_glass.snapshot import load_snapshot, scene_fingerprint


def _small_scene():
    description = build_default_scene()
    description["render_config"].update(
        {
            "image_width": 48,
            "image_height": 36,
            "tile_width": 24,
            "tile_height": 18,
            "sample_density": 8,
            "bubble_count": 4,
        }
    )
    return description, parse_scene(description)


class SnapshotResumeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="kiln_snap_")

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_interrupted_run_resumes_byte_identically(self):
        _, scene = _small_scene()
        interrupted_dir = os.path.join(self.tmp, "interrupted")
        renderer = Renderer(scene, interrupted_dir)
        total_tiles = len(renderer.tiles) * len(scene.view_angles)
        renderer.interrupt_after_tiles = max(1, total_tiles // 2)
        with self.assertRaises(GracefulInterrupt):
            renderer.run()

        snapshot = read_json(os.path.join(interrupted_dir, "progress_snapshot.json"))
        recovery = read_json(os.path.join(interrupted_dir, "recovery_report.json"))
        self.assertTrue(snapshot["interrupted"])
        self.assertEqual(recovery["status"], "resumable")
        self.assertGreater(len(snapshot["completed"]), 0)

        renderer2 = Renderer(scene, interrupted_dir)
        report = renderer2.run()
        self.assertTrue(report["restarted_from_snapshot"])

        reference_dir = os.path.join(self.tmp, "reference")
        Renderer(scene, reference_dir).run()
        differences = compare_runs(reference_dir, interrupted_dir)
        self.assertEqual(differences, [])

    def test_corrupt_snapshot_is_discarded_and_restarts(self):
        description, scene = _small_scene()
        out = os.path.join(self.tmp, "corrupt")
        renderer = Renderer(scene, out)
        renderer.interrupt_after_tiles = 2
        with self.assertRaises(GracefulInterrupt):
            renderer.run()
        snapshot_path = os.path.join(out, "progress_snapshot.json")
        with open(snapshot_path, "a", encoding="utf-8") as handle:
            handle.write("{not-json")
        state = load_snapshot(
            out,
            scene_fingerprint(
                canonical_scene_bytes(description),
                [b.fringe_seed for b in scene.bricks],
            ),
            Renderer(scene, out).summary,
        )
        self.assertIsNone(state)
        self.assertFalse(os.path.exists(snapshot_path))

    def test_seed_change_invalidates_snapshot(self):
        description, scene = _small_scene()
        out = os.path.join(self.tmp, "seed")
        renderer = Renderer(scene, out)
        renderer.interrupt_after_tiles = 1
        with self.assertRaises(GracefulInterrupt):
            renderer.run()

        changed = json.loads(json.dumps(description))
        changed["wall"]["bricks"][0]["fringe_seed"] += 999
        changed_scene = parse_scene(changed)
        fingerprint = scene_fingerprint(
            canonical_scene_bytes(changed),
            [b.fringe_seed for b in changed_scene.bricks],
        )
        state = load_snapshot(out, fingerprint, Renderer(changed_scene, out).summary)
        self.assertIsNone(state)

    def test_snapshot_summary_covers_scene_and_tile_colours(self):
        _, scene = _small_scene()
        out = os.path.join(self.tmp, "summary")
        renderer = Renderer(scene, out)
        renderer.interrupt_after_tiles = 3
        with self.assertRaises(GracefulInterrupt):
            renderer.run()
        snapshot = read_json(os.path.join(out, "progress_snapshot.json"))
        self.assertEqual(snapshot["scene_summary"]["brick_count"], len(scene.bricks))
        entry = next(iter(snapshot["completed"].values()))
        self.assertIn("sha256", entry["color"])
        self.assertEqual(entry["color"]["pixels"], 24 * 18)

    def test_missing_committed_tile_forces_full_restart(self):
        _, scene = _small_scene()
        out = os.path.join(self.tmp, "missing_tile")
        renderer = Renderer(scene, out)
        renderer.interrupt_after_tiles = 2
        with self.assertRaises(GracefulInterrupt):
            renderer.run()
        victim = os.path.join(out, "view_000", "tile_000.pam")
        self.assertTrue(os.path.exists(victim))
        os.remove(victim)
        report = Renderer(scene, out).run()
        self.assertEqual(len(report["views"]), len(scene.view_angles))
        self.assertFalse(
            os.path.exists(os.path.join(out, "progress_snapshot.json"))
        )


if __name__ == "__main__":
    unittest.main()
