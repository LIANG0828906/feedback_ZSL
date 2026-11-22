"""可选的 PyQt5 掩码查看器。

核心链路不依赖本模块；在无显示环境（CI/无头服务器）下设置
``QT_QPA_PLATFORM=offscreen`` 仍可渲染到图像，桌面环境则直接弹窗。
"""
from __future__ import annotations

import os
from typing import Optional

import numpy as np

from segworkbench.persistence.checkpoint import CheckpointStore
from segworkbench.pipeline.assemble import stitch_final_mask
from segworkbench.pipeline.loader import load_sample


def overlay_mask(image: np.ndarray, mask: np.ndarray, color=(255, 0, 0), alpha: int = 90) -> np.ndarray:
    """生成掩码叠加图（确定性，纯 numpy）。"""
    blended = image.copy()
    tint = np.array(color, dtype=np.uint8)
    overlay = np.where(mask[:, :, None], tint, image)
    return np.where(
        mask[:, :, None],
        ((image.astype(np.uint16) * (255 - alpha) + overlay.astype(np.uint16) * alpha) // 255).astype(np.uint8),
        blended,
    )


def render_to_file(data_dir: str, sample_id: str, checkpoint_root: str, output_path: str) -> str:
    """离屏渲染叠加图并通过内置 PNG 编码器写盘。"""
    sample = load_sample(data_dir, sample_id)
    mask = stitch_final_mask(CheckpointStore(checkpoint_root)).astype(bool)
    composed = overlay_mask(sample.image, mask)
    from segworkbench.utils.pngcodec import write_png

    write_png(output_path, composed)
    return output_path


def launch_viewer(data_dir: str, sample_id: str, checkpoint_root: str) -> int:
    """桌面弹窗查看器；返回应用退出码。"""
    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    from PyQt5.QtGui import QImage, QPixmap
    from PyQt5.QtWidgets import QApplication, QLabel, QMainWindow

    app = QApplication([])
    output_path = os.path.join(checkpoint_root, f"{sample_id}_overlay.png")
    render_to_file(data_dir, sample_id, checkpoint_root, output_path)
    window = QMainWindow()
    window.setWindowTitle(f"SegWorkbench - {sample_id}")
    label = QLabel()
    label.setPixmap(QPixmap.fromImage(QImage(output_path)))
    window.setCentralWidget(label)
    window.resize(540, 540)
    window.show()
    return app.exec_()
