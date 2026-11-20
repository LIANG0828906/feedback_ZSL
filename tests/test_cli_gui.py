from __future__ import annotations

import hashlib
from pathlib import Path

from streamforge.cli import main


def _files(root: Path) -> dict[str, str]:
    return {
        str(path.relative_to(root)): hashlib.sha256(path.read_bytes()).hexdigest()
        for path in sorted(root.rglob("*"))
        if path.is_file()
    }


def test_demo_logs_and_slices_are_byte_identical(tmp_path: Path) -> None:
    samples = tmp_path / "samples"
    first_output = tmp_path / "first"
    second_output = tmp_path / "second"
    assert main(
        [
            "demo",
            "--samples",
            str(samples),
            "--output",
            str(first_output),
            "--duration-ms",
            "1200",
        ]
    ) == 0
    assert main(
        [
            "demo",
            "--samples",
            str(samples),
            "--output",
            str(second_output),
            "--duration-ms",
            "1200",
        ]
    ) == 0
    assert (first_output / "demo_trace.jsonl").read_bytes() == (
        second_output / "demo_trace.jsonl"
    ).read_bytes()
    first_slices = {
        path.name: hashlib.sha256(path.read_bytes()).hexdigest()
        for path in first_output.rglob("*.fslice")
    }
    second_slices = {
        path.name: hashlib.sha256(path.read_bytes()).hexdigest()
        for path in second_output.rglob("*.fslice")
    }
    assert first_slices == second_slices
    snapshot_a = (first_output / "snapshot.json").read_bytes()
    snapshot_b = (second_output / "snapshot.json").read_bytes()
    assert snapshot_a == snapshot_b


def test_gui_window_smoke(tmp_path: Path) -> None:
    from PySide6.QtWidgets import QApplication

    from streamforge.gui import create_main_window

    app = QApplication.instance() or QApplication([])
    window = create_main_window(
        samples_root=tmp_path / "samples",
        output_root=tmp_path / "output",
        seed=1,
    )
    assert "StreamForge" in window.windowTitle()
    assert window.table.columnCount() == 4
    assert window.start_button.isEnabled()
    window.close()
    assert app is not None
