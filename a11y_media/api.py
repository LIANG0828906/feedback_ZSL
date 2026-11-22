"""可集成的领域组件接口(纯函数式查询)。

查询接口 "取某句台词窗口 / 取某字符高亮区间 / 取整工程误差报告"
只读取不可变结果快照, 不影响引擎内部状态。
"""

from __future__ import annotations

from . import engine as engine_mod
from .checkpoint import CheckpointStore
from .engine import Engine, ProjectResult
from .errors import schema_error
from .loader import load_project
from .model import Project


class MediaAccessibilityService:
    """领域组件门面: 加载工程、驱动三条处理路径、暴露纯查询接口。"""

    def __init__(self, project_dir: str, checkpoint_root: str | None = None,
                 max_workers: int = 2, buffer_limit: int = 4):
        self.project: Project = load_project(project_dir)
        self.store = CheckpointStore(checkpoint_root) if checkpoint_root else None
        self.max_workers = max_workers
        self.buffer_limit = buffer_limit
        self._result: ProjectResult | None = None

    def new_engine(self, **kwargs) -> Engine:
        return Engine(
            self.project,
            store=self.store,
            max_workers=self.max_workers,
            buffer_limit=self.buffer_limit,
            **kwargs,
        )

    def run_realtime(self, **kwargs) -> ProjectResult:
        self._result = self.new_engine(**kwargs).run()
        return self._result

    def run_full(self) -> ProjectResult:
        self._result = engine_mod.run_full(self.project)
        return self._result

    def set_result(self, result: ProjectResult) -> None:
        self._result = result

    def _require_result(self) -> ProjectResult:
        if self._result is None:
            raise schema_error("尚未产出结果, 请先运行任一路径")
        return self._result

    def get_window(self, sub_id: int) -> tuple:
        """取某句台词的显示窗口 [start, end)。"""
        result = self._require_result()
        if not (0 <= sub_id < len(result.windows)):
            raise schema_error("句号越界", sub=sub_id)
        return tuple(result.windows[sub_id])

    def get_char_interval(self, sub_id: int, char_idx: int) -> tuple:
        """取某字符的高亮区间 [start, end)。"""
        result = self._require_result()
        if not (0 <= sub_id < len(result.char_starts)):
            raise schema_error("句号越界", sub=sub_id)
        starts = result.char_starts[sub_id]
        if not (0 <= char_idx < len(starts)):
            raise schema_error("字符序号越界", sub=sub_id, char=char_idx)
        start = starts[char_idx]
        end = (
            starts[char_idx + 1]
            if char_idx + 1 < len(starts)
            else result.windows[sub_id][1]
        )
        return (start, end)

    def get_error_report(self) -> dict:
        """取整工程误差报告(深拷贝, 调用方修改不影响内部)。"""
        import copy

        return copy.deepcopy(self._require_result().report)
