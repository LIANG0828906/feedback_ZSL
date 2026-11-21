"""可选 PySide6 工作台界面。

界面只做薄封装，所有计算仍走包内确定性核心；
在无显示环境（如 CI）用 QT_QPA_PLATFORM=offscreen 即可实例化。
"""

from __future__ import annotations

from PySide6 import QtCore, QtWidgets

from .experiment import ExperimentRunner, TrajectoryLog
from .pipeline import ConversionConfig


class WorkbenchWindow(QtWidgets.QMainWindow):
    def __init__(self, work_dir: str, sequence_path: str):
        super().__init__()
        self.setWindowTitle("HDR 转换工作台")
        self.work_dir = work_dir
        self.sequence_path = sequence_path
        central = QtWidgets.QWidget()
        layout = QtWidgets.QVBoxLayout(central)
        self.info = QtWidgets.QLabel("尚未运行转换")
        self.run_button = QtWidgets.QPushButton("运行浮点 / 定点双路径")
        self.output = QtWidgets.QPlainTextEdit()
        self.output.setReadOnly(True)
        layout.addWidget(self.info)
        layout.addWidget(self.run_button)
        layout.addWidget(self.output)
        self.setCentralWidget(central)
        self.run_button.clicked.connect(self._run)

    @QtCore.Slot()
    def _run(self):
        log_path = f"{self.work_dir}/gui_trajectory.jsonl"
        log = TrajectoryLog(log_path)
        runner = ExperimentRunner(self.sequence_path, self.work_dir, log)
        config = ConversionConfig(
            target_colorspace=2,
            target_transfer=2,
            target_bit_depth=10,
            target_peak_nits=1000,
            tone_curve="reinhard",
            mode="fixed",
            frac_bits=14,
        )
        version_hash = runner.run_version(config)
        log.close()
        version = runner.store.get(version_hash)
        self.info.setText(f"当前版本 {version.version_id}: {version_hash[:12]}…")
        self.output.setPlainText(
            "\n".join(
                f"帧 {item.frame_index}: 最大双路径偏差 {item.max_diff_codes} 码值"
                for item in version.frames
            )
        )


def launch(work_dir: str, sequence_path: str) -> int:
    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
    window = WorkbenchWindow(work_dir, sequence_path)
    window.show()
    return app.exec()

