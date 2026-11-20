"""Optional PySide6 workbench; the CLI demo remains the headless equivalent."""

from __future__ import annotations

import argparse
import os
import sys
import threading
from pathlib import Path
from typing import Any


def _import_qt() -> tuple[Any, Any, Any, Any, Any, Any]:
    from PySide6.QtCore import QThread, QTimer, Signal
    from PySide6.QtWidgets import QApplication, QMainWindow, QPushButton

    return QThread, QTimer, Signal, QApplication, QMainWindow, QPushButton


def run_gui(work: Path, *, smoke: bool = False, offscreen: bool = False) -> int:
    if offscreen:
        os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    QThread, QTimer, Signal, QApplication, QMainWindow, QPushButton = _import_qt()
    from PySide6.QtWidgets import QFileDialog, QHBoxLayout, QLabel, QPlainTextEdit, QVBoxLayout, QWidget

    from .audio import generate_sample_set
    from .config import FingerprintConfig
    from .errors import BizError
    from .library import Library, TrackSpec
    from .migration import MigrationContext
    from .records import pcm_compression_metadata

    class ImportWorker(QThread):
        completed = Signal(object)
        failed = Signal(str)

        def __init__(self, library: Library, tracks: list[TrackSpec]) -> None:
            super().__init__()
            self.library = library
            self.tracks = tracks
            self.cancellation = threading.Event()

        def run(self) -> None:
            try:
                result = self.library.import_tracks(self.tracks, cancellation=self.cancellation)
                self.completed.emit(result.as_dict())
            except BizError as exc:
                self.failed.emit(exc.code + ": " + exc.message)
            except Exception as exc:  # pragma: no cover - defensive GUI boundary
                self.failed.emit(repr(exc))

    class Window(QMainWindow):
        def __init__(self) -> None:
            super().__init__()
            self.setWindowTitle("FPStore 本地指纹样本库")
            self.resize(980, 700)
            self.work = work
            self.samples = self.work / "samples"
            self.library = Library(self.work / "library")
            self.worker: ImportWorker | None = None
            self.config = FingerprintConfig(version=3, time_quant_bits=0)

            central = QWidget()
            layout = QVBoxLayout(central)
            row = QHBoxLayout()
            buttons = [
                ("生成样本", self.generate),
                ("开始入库", self.start_import),
                ("取消入库", self.cancel_import),
                ("恢复入库", self.resume_import),
                ("整体发布", self.publish),
                ("查询首样本", self.query),
                ("迁移到 v3", self.migrate),
                ("状态", self.show_status),
            ]
            for text, slot in buttons:
                button = QPushButton(text)
                button.clicked.connect(slot)
                row.addWidget(button)
            layout.addLayout(row)
            self.info = QLabel("工作目录: " + str(self.work))
            layout.addWidget(self.info)
            self.log = QPlainTextEdit()
            self.log.setReadOnly(True)
            layout.addWidget(self.log)
            self.setCentralWidget(central)
            self.write("GUI 已就绪；时间基准、量化和发布哈希均显示在轨迹中。")

        def write(self, text: str) -> None:
            self.log.appendPlainText(text)

        def generate(self) -> None:
            rows = generate_sample_set(self.samples)
            self.write(f"生成 {len(rows)} 个确定性 PCM16 WAV 样本")

        def _tracks(self) -> list[TrackSpec]:
            return [
                TrackSpec(path.stem, path, self.config)
                for path in sorted(self.samples.glob("*.wav"))
            ]

        def start_import(self) -> None:
            if self.worker is not None and self.worker.isRunning():
                self.write("E_BUSY: 当前入库任务尚未结束")
                return
            tracks = self._tracks()
            if not tracks:
                self.write("没有样本，请先生成样本")
                return
            self.worker = ImportWorker(self.library, tracks)
            self.worker.completed.connect(self._import_done)
            self.worker.failed.connect(lambda text: self.write("入库失败: " + text))
            self.worker.start()
            self.write("入库任务已启动，将在每个分段检查点响应取消")

        def cancel_import(self) -> None:
            if self.worker is not None and self.worker.isRunning():
                self.worker.cancellation.set()
                self.write("已请求协作式取消；已提交分段和上次发布库版本保留")
            else:
                self.write("当前没有运行中的入库任务")

        def _import_done(self, result: dict[str, Any]) -> None:
            self.write("入库检查点: " + str(result))

        def resume_import(self) -> None:
            try:
                result = self.library.import_tracks([], resume=True)
                self.write("恢复结果: " + str(result.as_dict()))
            except BizError as exc:
                self.write(exc.code + ": " + exc.message)

        def publish(self) -> None:
            try:
                result = self.library.publish()
                self.write("发布版本: " + str(result["published_version_hash"]))
            except BizError as exc:
                self.write(exc.code + ": " + exc.message)

        def query(self) -> None:
            paths = sorted(self.samples.glob("*.wav"))
            if not paths:
                self.write("没有可用于查询的样本")
                return
            try:
                result = self.library.search(paths[0], config=self.config, top_k=3)
                self.write(
                    "查询版本 "
                    + str(result["library_version_hash"])
                    + "；候选 "
                    + str(
                        [
                            (item["sample"], item["offset_ms"], item["confidence"])
                            for item in result["candidates"]
                        ]
                    )
                    + "；快照已释放="
                    + str(result["snapshot_released"])
                )
            except BizError as exc:
                self.write(exc.code + ": " + exc.message)

        def migrate(self) -> None:
            try:
                result = self.library.migrate(MigrationContext("not_recorded", pcm_compression_metadata(16)))
                self.write(
                    "迁移发布: "
                    + str(result["published_version_hash"])
                    + "；记录数="
                    + str(result["migrated_records"])
                )
            except BizError as exc:
                self.write(exc.code + ": " + exc.message)

        def show_status(self) -> None:
            self.write(str(self.library.status()))

    app = QApplication.instance() or QApplication(sys.argv[:1])
    window = Window()
    window.show()
    if smoke:
        QTimer.singleShot(150, app.quit)
    return int(app.exec())


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="fpstore-gui")
    parser.add_argument("--work", default="artifacts/gui")
    parser.add_argument("--smoke", action="store_true")
    parser.add_argument("--offscreen-smoke", action="store_true")
    args = parser.parse_args(argv)
    return run_gui(
        Path(args.work),
        smoke=args.smoke or args.offscreen_smoke,
        offscreen=args.offscreen_smoke,
    )


if __name__ == "__main__":
    raise SystemExit(main())
