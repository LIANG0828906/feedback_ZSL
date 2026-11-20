import json
import tempfile
import unittest
from pathlib import Path

from replay.generator import GenerationPlan, write_generation
from replay.models import canonical_dumps
from replay.parser import parse_replay
from replay.pipeline import ReplayPipeline


class CrossVersionTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.plan = GenerationPlan(room_id="room-x", seed=7, players=4, total_ticks=18, disorder=3)
        self.paths = write_generation(self.root / "sample", self.plan)

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def test_hash_chain_identical_across_versions(self):
        chains = {}
        summaries = []
        for version in (1, 2, 3):
            pipe = ReplayPipeline(
                snapshot_dir=self.root / f"snap{version}",
                baseline_path=self.paths["baseline"],
            )
            result = pipe.run(self.paths[f"v{version}"])
            self.assertIsNone(result.fork)
            chains[version] = result.hash_chain
            summaries.append(result.to_summary())
        self.assertEqual(chains[1], chains[2])
        self.assertEqual(chains[2], chains[3])

    def test_state_bytes_identical_across_versions(self):
        state_bytes = {}
        for version in (1, 2, 3):
            pipe = ReplayPipeline(snapshot_dir=self.root / f"bytes{version}")
            result = pipe.run(self.paths[f"v{version}"])
            state_bytes[version] = [canonical_dumps(s.canonical_payload()) for _, s in sorted(result.states.items())]
        self.assertEqual(state_bytes[1], state_bytes[2])
        self.assertEqual(state_bytes[2], state_bytes[3])

    def test_snapshot_source_annotation(self):
        v1 = parse_replay(self.paths["v1"])
        self.assertEqual(v1.snapshots, {})
        pipe = ReplayPipeline(snapshot_dir=self.root / "src1")
        result = pipe.run(self.paths["v1"])
        self.assertTrue(all(v.startswith("derived") for v in result.snapshot_sources.values()))
        pipe3 = ReplayPipeline(snapshot_dir=self.root / "src3")
        result3 = pipe3.run(self.paths["v3"])
        self.assertTrue(all("v3-diff" in v for v in result3.snapshot_sources.values()))

    def test_deterministic_regeneration_byte_identical(self):
        again = self.root / "again"
        write_generation(again, self.plan)
        for name in ("replay_v1.json", "replay_v2.json", "replay_v3.json", "baseline.json", "trace.log"):
            first = (self.root / "sample" / name).read_bytes()
            second = (again / name).read_bytes()
            self.assertEqual(first, second, name)

    def test_format_invalid_locates_record_and_path(self):
        from replay.errors import ErrorCode, ReplayError

        replay = json.loads(Path(self.paths["v2"]).read_text())
        del replay["records"][3]["snapshot"]["resources"]
        with self.assertRaises(ReplayError) as ctx:
            parse_replay(replay)
        self.assertEqual(ctx.exception.code, ErrorCode.E_FORMAT_INVALID)
        self.assertEqual(ctx.exception.record_index, 3)
        self.assertIn("resources", ctx.exception.field_path)

    def test_unknown_version_code(self):
        from replay.errors import ErrorCode, ReplayError

        replay = json.loads(Path(self.paths["v1"]).read_text())
        replay["format_version"] = 9
        with self.assertRaises(ReplayError) as ctx:
            parse_replay(replay)
        self.assertEqual(ctx.exception.code, ErrorCode.E_FORMAT_VERSION_UNKNOWN)


if __name__ == "__main__":
    unittest.main()
