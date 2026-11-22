"""有界线程池与每块缓冲上限。

最大并行度由调用方固定；线程池只负责并行计算，所有检查点写入在
:class:`CheckpointStore` 的锁内串行完成。取消通过协作式 Event 在块
边界生效，不中断已经在写的检查点。
"""
from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Callable, List, Sequence, TypeVar

from segworkbench.core.errors import ErrorCode, WorkbenchError, fail

T = TypeVar("T")
R = TypeVar("R")


class BoundedRunner:
    def __init__(self, max_workers: int = 2, max_buffered_per_block: int = 1) -> None:
        if int(max_workers) < 1 or int(max_workers) > 16:
            raise fail(ErrorCode.E_PARA, "max_workers 越界", field="max_workers", value=max_workers)
        if int(max_buffered_per_block) < 1 or int(max_buffered_per_block) > 8:
            raise fail(ErrorCode.E_PARA, "max_buffered_per_block 越界",
                       field="max_buffered_per_block", value=max_buffered_per_block)
        self.max_workers = int(max_workers)
        # 每个块同一时刻最多保留 max_buffered_per_block 份未释放阶段缓冲。
        self.max_buffered_per_block = int(max_buffered_per_block)

    def map(self, func: Callable[[T], R], items: Sequence[T]) -> List[R]:
        """按输入顺序返回结果；异常按块隔离后聚合重抛首个业务错误。"""
        results: List[R | Exception] = [None] * len(items)  # type: ignore[assignment]
        errors: List[WorkbenchError] = []
        with ThreadPoolExecutor(max_workers=self.max_workers,
                                thread_name_prefix="seg-block") as executor:
            future_to_index = {executor.submit(func, item): index for index, item in enumerate(items)}
            for future in as_completed(future_to_index):
                index = future_to_index[future]
                try:
                    results[index] = future.result()
                except WorkbenchError as exc:
                    errors.append(exc)
                    results[index] = exc
        if errors:
            ordered = sorted(errors, key=lambda exc: (exc.context.get("block_id", ""),
                                                       exc.context.get("phase", "")))
            raise ordered[0]
        return results  # type: ignore[return-value]
