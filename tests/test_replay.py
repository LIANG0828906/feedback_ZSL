from __future__ import annotations

import json
import shutil
import tempfile
import unittest
from pathlib import Path

from tickarena import (
    E_DIVERGE,
    E_OK,
    E_REPLAY,
    Engine,
    JsonlEventSource,
    verify_replay,
)
from tickarena.replay import load_and_validate_replay

from tests.support import rewrite_replay, write_match


class ReplayTests(unittest.TestCase):
    def _record(self, tmp: str):
        initial, event_path = write_match(
            tmp,
            seed=123,
            players=4,
            ticks=180,
            density="1.5",
        )
        replay = Path(tmp) / "match.replay.jsonl"
        source = JsonlEventSource(event_path)
        engine = Engine(
            initial,
            source,
            replay_path=replay,
            snapshot_interval=30,
        )
        while not engine.done:
            self.assertEqual(engine.step().code, E_OK)
        engine.close()
        return replay, event_path

    def test_independent_verification_succeeds(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            replay, event_path = self._record(tmp)
            meta = load_and_validate_replay(replay)
            self.assertEqual(meta.header["format_version"], 1)
            report = verify_replay(replay, event_path)
            self.assertEqual(report.code, E_OK, report)
            self.assertEqual(report.tick, meta.footer["final_tick"])

    def test_truncated_replay_returns_e_replay(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            replay, event_path = self._record(tmp)
            damaged = Path(tmp) / "damaged.replay.jsonl"
            data = replay.read_bytes()
            damaged.write_bytes(data[:-17])
            report = verify_replay(damaged, event_path)
            self.assertEqual(report.code, E_REPLAY)

    def test_unknown_version_returns_e_replay(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            replay, event_path = self._record(tmp)
            lines = replay.read_bytes().splitlines(keepends=True)
            header = json.loads(lines[0])
            header["format_version"] = 999
            lines[0] = (
                json.dumps(
                    header,
                    ensure_ascii=False,
                    sort_keys=True,
                    separators=(",", ":"),
                ).encode("utf-8")
                + b"\n"
            )
            bad = Path(tmp) / "future.replay.jsonl"
            bad.write_bytes(b"".join(lines))
            report = verify_replay(bad, event_path)
            self.assertEqual(report.code, E_REPLAY)

    def test_semantic_tamper_reports_earliest_tick_and_rule(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            replay, event_path = self._record(tmp)

            def mutate(body):
                for record in body:
                    if record.get("kind") == "tick" and record.get("tick") == 3:
                        record["units"]["u1"]["hp"] += 7
                        break
                return body

            rewrite_replay(replay, mutate)
            report = verify_replay(replay, event_path)
            self.assertEqual(report.code, E_DIVERGE)
            self.assertEqual(report.tick, 3)
            self.assertEqual(report.rule, "units.u1.hp")
            self.assertEqual(
                report.recomputed,
                report.recorded - 7,
            )

    def test_rejection_code_tamper_is_detected(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            initial, event_path = write_match(
                tmp,
                seed=99,
                players=2,
                ticks=20,
                density="0",
            )
            events = [
                {"tick": 0, "type": "skill", "actor": "u1", "skill": "bolt", "target": "u2"},
                {"tick": 0, "type": "skill", "actor": "u1", "skill": "bolt", "target": "u2"},
            ]
            # Keep the file-backed source identity used by the replay fingerprint.
            from tickarena.events import write_event_stream

            write_event_stream(event_path, initial, iter(events))
            replay = Path(tmp) / "rejections.replay.jsonl"
            source = JsonlEventSource(event_path)
            engine = Engine(initial, source, replay_path=replay, snapshot_interval=10)
            while not engine.done:
                engine.step()
            engine.close()

            def mutate(body):
                for record in body:
                    if record.get("kind") == "tick" and record.get("events"):
                        for summary in record["events"]:
                            if summary.get("code") == "E_COOLDOWN":
                                summary["code"] = None
                                return body
                self.fail("expected an E_COOLDOWN summary")

            rewrite_replay(replay, mutate)
            report = verify_replay(replay, event_path)
            self.assertEqual(report.code, E_DIVERGE)
            self.assertEqual(report.tick, 0)
            self.assertIn("events", report.rule or "")

    def test_wrong_event_stream_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            replay, _event_path = self._record(tmp)
            other_dir = Path(tmp) / "other"
            _initial, other_path = write_match(
                other_dir,
                seed=124,
                players=4,
                ticks=180,
                density="1.5",
            )
            report = verify_replay(replay, other_path)
            self.assertEqual(report.code, E_REPLAY)


if __name__ == "__main__":
    unittest.main()

