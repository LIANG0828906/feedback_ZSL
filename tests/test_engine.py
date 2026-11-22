from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from tickarena import (
    E_COOLDOWN,
    E_FINISHED,
    E_NUM,
    E_OK,
    E_ORDER,
    E_RES,
    E_UNIT,
    Engine,
    InMemoryEventSource,
    verify_replay,
)
from tickarena.codes import TickArenaError
from tickarena.model import parse_initial_state
from tickarena.rng import derive_u64

from tests.support import write_match


class EngineRuleTests(unittest.TestCase):
    def _two_player_state(self, tmp: str):
        initial, _event_path = write_match(
            tmp,
            seed=91,
            players=2,
            ticks=12,
            density="0",
            win_score=100,
        )
        return initial

    def test_required_event_rejections_and_replay_consistency(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            initial = self._two_player_state(tmp)
            initial["players"][1]["unit"]["resource"] = 0
            events = [
                {"tick": 0, "type": "skill", "actor": "u1", "skill": "bolt", "target": "u2"},
                {"tick": 0, "type": "skill", "actor": "u1", "skill": "bolt", "target": "u2"},
                {"tick": 0, "type": "skill", "actor": "u2", "skill": "bolt", "target": "u1"},
                {"tick": 0, "type": "skill", "actor": "ghost", "skill": "bolt", "target": "u1"},
                {"tick": 0, "type": "move", "actor": "u1", "dx": 1.0, "dy": 0},
                {"tick": 2, "type": "move", "actor": "u1", "dx": 1, "dy": 0},
                {"tick": 1, "type": "move", "actor": "u1", "dx": 1, "dy": 0},
            ]
            source = InMemoryEventSource(initial, events)
            replay = Path(tmp) / "rules.replay.jsonl"
            engine = Engine(initial, source, replay_path=replay, snapshot_interval=2)
            codes: list[str] = []
            while not engine.done:
                result = engine.step()
                self.assertEqual(result.code, E_OK)
                codes.extend(summary["code"] for summary in result.rejected)
            self.assertIn(E_COOLDOWN, codes)
            self.assertIn(E_RES, codes)
            self.assertIn(E_UNIT, codes)
            self.assertIn(E_ORDER, codes)
            self.assertIn(E_NUM, codes)
            report = verify_replay(replay, source)
            self.assertEqual(report.code, E_OK, report)

    def test_score_result_locks_and_final_event_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            initial = self._two_player_state(tmp)
            initial["rules"]["win_score"] = 1
            initial["players"][1]["unit"]["hp"] = 5
            initial["players"][1]["unit"]["max_hp"] = 5
            initial["players"][0]["unit"]["skills"]["bolt"].update(
                hit_bps=10_000,
                crit_bps=0,
            )
            events = [
                {"tick": 0, "type": "skill", "actor": "u1", "skill": "bolt", "target": "u2"},
                {"tick": 0, "type": "gather", "actor": "u2"},
            ]
            source = InMemoryEventSource(initial, events)
            replay = Path(tmp) / "win.replay.jsonl"
            engine = Engine(initial, source, replay_path=replay, snapshot_interval=5)
            first = engine.step()
            self.assertTrue(first.finished)
            self.assertEqual(engine.state.result["reason"], "score_limit")
            self.assertEqual(engine.state.result["winner_team"], "red")
            self.assertEqual(first.rejected[0]["code"], E_FINISHED)
            locked = dict(engine.state.result)
            again = engine.step()
            self.assertTrue(again.finished)
            self.assertEqual(engine.state.result, locked)
            report = verify_replay(replay, source)
            self.assertEqual(report.code, E_OK, report)

    def test_death_revive_resets_position_and_cooldown(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            initial = self._two_player_state(tmp)
            initial["players"][1]["unit"]["hp"] = 5
            initial["players"][1]["unit"]["max_hp"] = 10
            initial["players"][1]["unit"]["revive_ticks"] = 3
            initial["players"][0]["unit"]["skills"]["bolt"].update(
                hit_bps=10_000,
                crit_bps=0,
            )
            target_initial = initial["players"][1]["unit"]
            events = [
                {"tick": 0, "type": "skill", "actor": "u1", "skill": "bolt", "target": "u2"},
                {"tick": 1, "type": "move", "actor": "u2", "dx": 1, "dy": 0},
            ]
            source = InMemoryEventSource(initial, events)
            engine = Engine(initial, source)
            for _ in range(4):
                engine.step()
            target = engine.state.units["u2"]
            self.assertTrue(target.alive)
            self.assertEqual(target.hp, 5)
            self.assertEqual(
                (target.x, target.y),
                (target_initial["spawn_x"], target_initial["spawn_y"]),
            )
            self.assertEqual(target.cooldowns["bolt"], 0)
            self.assertEqual(engine.state.rejected_total, 1)

    def test_float_initial_state_returns_e_num(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            initial = self._two_player_state(tmp)
            initial["players"][0]["unit"]["x"] = 1.0
            with self.assertRaises(TickArenaError) as caught:
                parse_initial_state(initial)
            self.assertEqual(caught.exception.code, E_NUM)

    def test_rng_coordinate_is_path_independent(self) -> None:
        first = derive_u64(123, 77, 9, "skill:bolt:hit")
        second = derive_u64(123, 77, 9, "skill:bolt:hit")
        other_tick = derive_u64(123, 78, 9, "skill:bolt:hit")
        self.assertEqual(first, second)
        self.assertNotEqual(first, other_tick)


if __name__ == "__main__":
    unittest.main()

