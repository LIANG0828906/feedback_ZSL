from syncdemo import runner
from syncdemo.__main__ import main
from syncdemo.checkpoint import CheckpointStore


def _store(tmp_path, sample, name):
    return CheckpointStore(
        tmp_path / name,
        seed=sample.meta["seed"],
        input_fingerprint=sample.meta["input_fingerprint"],
        algo_version=sample.meta["algo_version"],
    )


def test_runner_reports_delay_violations(sample):
    result = runner.run_replay(sample)
    assert result.input_delay_violations == []
    assert result.correction_delay_violations == []


def test_resume_restores_delay_tracking(sample, tmp_path):
    rcfg = runner.RunConfig(window_ticks=15)
    store = _store(tmp_path, sample, "s")
    partial = runner.run_realtime(sample, rcfg, store=store,
                                  cancel_after_tick=sample.max_tick // 2)
    assert partial.status == "cancelled"
    resumed = runner.run_resume(sample, rcfg, store=store)
    realtime = runner.run_realtime(sample, rcfg)
    assert resumed.input_delay_violations == realtime.input_delay_violations
    assert (resumed.correction_delay_violations
            == realtime.correction_delay_violations)


def test_verify_command_reports_delay_violations(sample_dir, tmp_path,
                                                 capsys):
    rc = main(["verify", "--sample", str(sample_dir),
               "--work", str(tmp_path / "work")])
    assert rc == 0
    out = capsys.readouterr().out
    assert "truth check: OK" in out
    assert "input delay violations: none" in out
    assert "correction delay violations: none" in out
