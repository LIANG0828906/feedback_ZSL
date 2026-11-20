from __future__ import annotations

from dustrender.acceptance import run_acceptance


def test_complete_acceptance_suite(tmp_path) -> None:
    report = run_acceptance(tmp_path / "acceptance", seed=20261121)
    assert report["ok"] is True
    assert all(check["status"] == "passed" for check in report["checks"])
