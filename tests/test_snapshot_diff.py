import json
import tempfile
import unittest
from pathlib import Path

from replay.generator import GenerationPlan, write_generation
from replay.diff_codec import decode_diff, encode_diff
from replay.errors import ErrorCode, ReplayError
from replay.models import RoomState
from replay.parser import parse_replay
from replay.pipeline import ReplayPipeline
from replay import snapshot as snapshot_mod


class SnapshotRecoveryTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.paths = write_generation(
            self.root / "sample",
            GenerationPlan(room_id="room-r", seed=11, players=2, total_ticks=10, disorder=2),
        )

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def _verify(self, snap_dir):
        replay = parse_replay(self.paths["v2"])
        return snapshot_mod.verify_and_recover(
            snap_dir, replay.initial_state, replay.operations, replay.late_window
        )

    def test_atomic_snapshots_no_tmp_residue(self):
        snap_dir = self.root / "snap"
        pipe = ReplayPipeline(snapshot_dir=snap_dir)
        pipe.run(self.paths["v2"])
        files = list(snap_dir.iterdir())
        self.assertTrue(files)
        self.assertFalse(any(p.name.startswith(".") for p in files))

    def test_recovery_resumes_without_reapplying(self):
        snap_dir = self.root / "snap"
        first = ReplayPipeline(snapshot_dir=snap_dir, snapshot_every=1)
        result_first = first.run(self.paths["v2"])
        full_chain = list(result_first.hash_chain)

        recovery = self._verify(snap_dir)
        self.assertIsNotNone(recovery)
        self.assertEqual(recovery.last_tick, 10)

        second = ReplayPipeline(snapshot_dir=snap_dir, snapshot_every=1)
        result_second = second.run(self.paths["v2"])
        self.assertEqual(result_second.recovery_tick, 10)
        # 已到末尾，恢复后不产生新步，也不重复应用历史操作。
        self.assertEqual(result_second.hash_chain, [])
        recovery = self._verify(snap_dir)
        self.assertEqual(
            canonical(recovery.engine.state),
            canonical(result_first.states[10]),
        )
        self.assertEqual(full_chain, result_first.hash_chain)

    def test_recovery_from_partial_run(self):
        snap_dir = self.root / "partial"
        pipe = ReplayPipeline(snapshot_dir=snap_dir, snapshot_every=2)
        pipe.run(self.paths["v1"])
        # 删除后半段快照，模拟强制终止只提交了部分步。
        for path in snapshot_mod.list_committed_snapshots(snap_dir):
            tick = int(path.stem.split("_")[1])
            if tick > 6:
                path.unlink()
        result = ReplayPipeline(snapshot_dir=snap_dir, snapshot_every=2).run(self.paths["v1"])
        self.assertEqual(result.recovery_tick, 6)
        self.assertEqual(len(result.states), 4)  # tick 7..10

    def test_corrupt_snapshot_chain_rejected(self):
        snap_dir = self.root / "broken"
        ReplayPipeline(snapshot_dir=snap_dir).run(self.paths["v2"])
        target = snap_dir / "snapshot_00000005.json"
        payload = json.loads(target.read_text())
        payload["chain_prefix"] = "f" * 64
        payload["prev_hash"] = "f" * 64
        target.write_text(json.dumps(payload), encoding="utf-8")
        with self.assertRaises(ReplayError) as ctx:
            self._verify(snap_dir)
        self.assertEqual(ctx.exception.code, ErrorCode.E_RECOVERY_INVALID)
        self.assertEqual(ctx.exception.record_index, 5)

    def test_missing_snapshot_field_rejected(self):
        snap_dir = self.root / "missing"
        ReplayPipeline(snapshot_dir=snap_dir).run(self.paths["v2"])
        target = snap_dir / "snapshot_00000003.json"
        payload = json.loads(target.read_text())
        del payload["water_level"]
        target.write_text(json.dumps(payload), encoding="utf-8")
        with self.assertRaises(ReplayError) as ctx:
            self._verify(snap_dir)
        self.assertEqual(ctx.exception.code, ErrorCode.E_RECOVERY_INVALID)


class DiffCodecTest(unittest.TestCase):
    def test_round_trip_diff(self):
        base = RoomState(tick=0, entities={"e0": {"pos": [0.0, 0.0], "vel": [0.0, 0.0], "alive": True, "owner": 0}}, resources={"ammo": 5})
        target = base.clone()
        target.tick = 1
        target.entities["e0"]["pos"] = [2.0, 1.0]
        target.resources["ammo"] = 4
        diff = encode_diff(base, target)
        paths = [change["path"] for change in diff["changes"]]
        self.assertEqual(paths, sorted(paths))
        decoded = decode_diff(base, diff, index=1, target_tick=1)
        self.assertEqual(decoded.canonical_payload(), target.canonical_payload())

    def test_unresolvable_base_tick_aborts(self):
        base = RoomState(tick=3, entities={}, resources={})
        with self.assertRaises(ReplayError) as ctx:
            decode_diff(base, {"base_tick": 9, "changes": []}, index=2, target_tick=4)
        self.assertEqual(ctx.exception.code, ErrorCode.E_DIFF_INVALID)

    def test_unknown_field_path_aborts(self):
        base = RoomState(tick=0, entities={}, resources={"ammo": 1})
        diff = {"base_tick": 0, "changes": [{"path": "resources.gold", "value": 1}]}
        with self.assertRaises(ReplayError) as ctx:
            decode_diff(base, diff, index=0, target_tick=1)
        self.assertEqual(ctx.exception.code, ErrorCode.E_DIFF_INVALID)

    def test_v3_bad_field_order_rejected_at_parse(self):
        replay = json.loads(Path(self.paths_v3()).read_text())
        replay["field_order"] = list(reversed(replay["field_order"]))
        with self.assertRaises(ReplayError) as ctx:
            parse_replay(replay)
        self.assertEqual(ctx.exception.code, ErrorCode.E_DIFF_INVALID)

    def test_v3_out_of_range_base_tick_aborts_pipeline(self):
        replay = json.loads(Path(self.paths_v3()).read_text())
        index = next(i for i, record in enumerate(replay["records"]) if "diff" in record)
        replay["records"][index]["diff"]["base_tick"] = 99
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(ReplayError) as ctx:
                ReplayPipeline(snapshot_dir=Path(tmp) / "snap").run(replay)
        self.assertEqual(ctx.exception.code, ErrorCode.E_DIFF_INVALID)
        self.assertIn("base_tick", ctx.exception.field_path)

    def paths_v3(self) -> Path:
        if not hasattr(self, "_v3"):
            self._tmp = tempfile.TemporaryDirectory()
            self.addCleanup(self._tmp.cleanup)
            self._v3 = write_generation(
                Path(self._tmp.name) / "s",
                GenerationPlan(room_id="d", seed=3, players=2, total_ticks=6),
            )["v3"]
        return self._v3


def canonical(state: RoomState) -> bytes:
    from replay.models import canonical_dumps

    return canonical_dumps(state.canonical_payload()) if state else b""


if __name__ == "__main__":
    unittest.main()
