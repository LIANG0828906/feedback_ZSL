from __future__ import annotations

import threading
import unittest
from unittest.mock import patch

from scworkbench.concurrency import BoundedExecutor, BufferLimitError
from scworkbench.engine import EngineConfig, SessionEngine
from scworkbench.loader import parse_session
from scworkbench.rendering import render_scene

from tests.helpers import sample_data


class RuntimeControlTests(unittest.TestCase):
    def test_tier_and_retry_changes_do_not_rewrite_committed_frames(self) -> None:
        sample = parse_session(sample_data())
        with SessionEngine(
            sample,
            config=EngineConfig(
                checkpoint_every=0,
                initial_tier="h264-baseline",
            ),
        ) as engine:
            engine.run_until_committed(10)
            before = [
                item.to_dict() for item in engine.to_run_result("before").frames
            ]
            before_stats = engine.stats.to_dict()
            engine.set_runtime_tier("h264-high")
            engine.set_retry_limit(6)
            engine.run_until_committed(15)
            result = engine.to_run_result("runtime")
        self.assertEqual(
            [item.to_dict() for item in result.frames[:10]],
            before,
        )
        self.assertTrue(
            all(item.codec == "h264-baseline" for item in result.frames[:10])
        )
        self.assertTrue(
            all(item.codec == "h264-high" for item in result.frames[10:15])
        )
        self.assertEqual(result.stats.packets_sent >= before_stats["packets_sent"], True)
        self.assertEqual(before[0]["fragment_status"], result.frames[0].fragment_status)

    def test_single_frame_exception_is_retried_without_polluting_neighbors(self) -> None:
        sample = parse_session(sample_data())
        calls = 0

        def flaky(*args, **kwargs):
            nonlocal calls
            calls += 1
            if calls == 1:
                raise RuntimeError("injected renderer failure")
            return render_scene(*args, **kwargs)

        with (
            patch("scworkbench.engine.render_scene", side_effect=flaky),
            SessionEngine(sample, EngineConfig(checkpoint_every=0)) as engine,
        ):
            engine.run_until_committed(2)
            result = engine.to_run_result("frame-retry")
        self.assertEqual(calls >= 2, True)
        self.assertEqual(len(result.frames), 2)
        self.assertEqual(result.frames[0].status, "committed")
        self.assertEqual(result.frames[1].status, "committed")
        self.assertEqual(result.stats.frames_failed, 0)
        self.assertEqual(result.stats.frame_retries, 1)


class BoundedConcurrencyTests(unittest.TestCase):
    def test_pending_buffer_is_hard_bounded(self) -> None:
        executor = BoundedExecutor(max_workers=2, max_pending=3, label="test")
        release = threading.Event()

        def block() -> None:
            release.wait(timeout=2)

        futures = [executor.submit(block) for _ in range(3)]
        with self.assertRaises(BufferLimitError):
            executor.submit(block)
        release.set()
        for future in futures:
            future.result(timeout=2)
        executor.shutdown()


if __name__ == "__main__":
    unittest.main()
