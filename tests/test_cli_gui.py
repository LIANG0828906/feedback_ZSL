from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from panoworkbench.cli import main


def test_cli_validate_and_run(sample_root: Path, tmp_path: Path, capsys) -> None:
    assert main(["validate", "--dataset", str(sample_root)]) == 0
    validation = json.loads(capsys.readouterr().out)
    assert validation["status"] == "ok"
    code = main(
        [
            "run",
            "--dataset",
            str(sample_root),
            "--output",
            str(tmp_path / "out"),
            "--no-resume",
        ]
    )
    assert code == 0
    output = json.loads(capsys.readouterr().out)
    assert output["status"] == "completed"
    assert output["panorama"]["matches_theoretical"] is True
    assert (tmp_path / "out" / "report.json").is_file()
    assert (tmp_path / "out" / "panorama.png").is_file()


def test_gui_constructs_offscreen(sample_root: Path) -> None:
    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    pytest.importorskip("PyQt5")
    from PyQt5.QtWidgets import QApplication

    from panoworkbench.gui import MainWindow

    app = QApplication.instance() or QApplication([])
    window = MainWindow(str(sample_root))
    assert window.dataset is not None
    assert window.dataset.image_count == 3
    assert "truth canvas" in window.dataset_label.text()
    window.close()
    app.processEvents()
