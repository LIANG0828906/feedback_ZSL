from __future__ import annotations

import contextlib
import io
import json
from pathlib import Path

from shadowbox.cli import main
from shadowbox.scene import generate_scene


def write_scene_dict(path: Path, scene: dict) -> None:
    path.write_text(json.dumps(scene, ensure_ascii=False), encoding="utf-8")


def small_scene() -> dict:
    return generate_scene(
        width=96,
        height=72,
        block_size=(32, 24),
        samples_per_pixel=2,
    )


def invoke_cli(arguments: list[str]) -> tuple[int, str, str]:
    stdout = io.StringIO()
    stderr = io.StringIO()
    with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
        code = main(arguments)
    return code, stdout.getvalue(), stderr.getvalue()
