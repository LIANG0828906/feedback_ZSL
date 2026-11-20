"""确定性资源计量与预算强制。

计量值由对局输入（回合号、玩家、操作类型）经固定公式推导，
不读取系统时间、不读取进程 RSS，因此同输入在任何机器上结果一致。
每个资源检查点都会触发协作式取消回调。
"""

from __future__ import annotations

from collections.abc import Callable

from .errors import BudgetExceeded
from .model import ResourceUsage

# 每种操作的基础算力/内存消耗表，属于规则引擎的一部分，固定不可配置。
ACTION_COST_TABLE = {
    "attack": {"compute": 40, "memory": 30},
    "defend": {"compute": 20, "memory": 25},
    "charge": {"compute": 15, "memory": 20},
    "skill": {"compute": 65, "memory": 55},
}


class ResourceMeter:
    """累计计量器：累计超预算立即抛出 BudgetExceeded，绝不静默降级。"""

    def __init__(
        self,
        compute_budget: int,
        memory_budget: int,
        usage: ResourceUsage | None = None,
        cancel_checkpoint: Callable[[], None] | None = None,
    ) -> None:
        self.compute_budget = int(compute_budget)
        self.memory_budget = int(memory_budget)
        self.usage = usage or ResourceUsage()
        self._cancel_checkpoint = cancel_checkpoint

    def _check_cancel(self) -> None:
        if self._cancel_checkpoint is not None:
            self._cancel_checkpoint()

    @staticmethod
    def action_cost(action_type: str, turn: int, player_id: int) -> tuple[int, int]:
        base = ACTION_COST_TABLE[action_type]
        # 固定公式产生小幅波动，只依赖输入，保证跨平台逐字节一致。
        compute = base["compute"] + (turn * 7 + player_id * 3) % 5
        memory = base["memory"] + (turn * 5 + player_id * 11) % 7
        return compute, memory

    def sample(self, turn: int, player_id: int, action_type: str) -> tuple[int, int]:
        """资源检查点：先响应取消，再计量并强制预算。"""
        self._check_cancel()
        compute, memory = self.action_cost(action_type, turn, player_id)
        new_compute = self.usage.compute_total + compute
        new_memory = self.usage.memory_total + memory
        if new_compute > self.compute_budget:
            raise BudgetExceeded(
                f"累计算力 {new_compute} 超过预算 {self.compute_budget}"
            )
        if new_memory > self.memory_budget:
            raise BudgetExceeded(
                f"累计内存 {new_memory} 超过内存预算 {self.memory_budget}"
            )
        self.usage.add(turn, compute, memory)
        self._check_cancel()
        return compute, memory
