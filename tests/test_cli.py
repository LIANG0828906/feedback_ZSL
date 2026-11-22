from __future__ import annotations

import json

from diffrender.cli import main


def test_cli_success_and_business_error_codes(dataset, tmp_path, capsys):
    code = main(
        [
            "strategy",
            "--dataset",
            str(dataset.root),
            "--group",
            "single",
        ]
    )
    assert code == 0
    captured = capsys.readouterr()
    payload = json.loads(captured.out)
    assert payload["ok"] is True

    code = main(
        [
            "run",
            "--dataset",
            str(dataset.root),
            "--group",
            "missing",
            "--out",
            str(tmp_path / "run"),
        ]
    )
    assert code == 2
    captured = capsys.readouterr()
    payload = json.loads(captured.err)
    assert payload["code"] == "E_SCHEMA"

