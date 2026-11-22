import os

import pytest

from syncdemo import generator


@pytest.fixture()
def ui_sample(tmp_path):
    return generator.generate(outdir=tmp_path / "sample", ticks=30)


def test_headless_demo_runs(ui_sample):
    os.environ["SDL_VIDEODRIVER"] = "dummy"
    from syncdemo.ui import DemoApp

    app = DemoApp(ui_sample, headless=True)
    app.speed = 120.0  # fast-forward so commit windows get locked
    app.run(max_frames=30)
    assert app.result is not None
    modes = {rec.mode for rec in app.result.frame_log}
    assert modes <= {"predicted", "authoritative"}
    assert app.committed_hashes, "expected committed checkpoint windows"


def test_ui_injection_rejected_on_committed_tick(ui_sample):
    os.environ["SDL_VIDEODRIVER"] = "dummy"
    from syncdemo.ui import DemoApp

    app = DemoApp(ui_sample, headless=True)
    app.tick_pos = 45.0            # play past window 1 (ticks 1..30)
    app._commit_played_windows()
    app.tick_pos = 10.0            # seek back into the committed range
    app._inject_input()
    assert "rejected" in app.alert
    app.tick_pos = 31.0            # window 2 not yet committed
    app._inject_input()
    assert "injected" in app.alert
    app.pygame.quit()


def test_ui_latency_adjust_and_replay(ui_sample):
    os.environ["SDL_VIDEODRIVER"] = "dummy"
    from syncdemo.ui import DemoApp

    app = DemoApp(ui_sample, headless=True)
    cid = app.sample.clients[0]["id"]
    app.latency_offset[cid] = 2
    app._rerun()
    assert app.alert == ""
    app._handle_key(app.pygame.K_r)  # trigger replay
    assert "replayed" in app.alert or app.alert == ""
    app.pygame.quit()
