"""业务错误码与回放领域异常。

所有错误码均为稳定字符串常量，CLI 与 GUI 均依据这些码给出诊断信息，
不允许通过异常消息文本进行分支判断。
"""

from __future__ import annotations


OK = "OK"
E_BUSY = "E_BUSY"
E_BUDGET_EXCEEDED = "E_BUDGET_EXCEEDED"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_BASELINE_MISMATCH = "E_BASELINE_MISMATCH"
E_CANCELLED = "E_CANCELLED"
E_NOT_FOUND = "E_NOT_FOUND"
E_INVALID_INPUT = "E_INVALID_INPUT"


ERROR_MESSAGES = {
    OK: "成功",
    E_BUSY: "同一回放任务重复提交",
    E_BUDGET_EXCEEDED: "任务资源消耗超出预算",
    E_RECOVERY_INVALID: "快照哈希链校验失败或快照缺失",
    E_BASELINE_MISMATCH: "基线版本与回放器版本不匹配",
    E_CANCELLED: "任务在检查点被协作式取消",
    E_NOT_FOUND: "对象不存在",
    E_INVALID_INPUT: "输入描述非法",
}


class ReplayError(Exception):
    """携带稳定业务码的领域异常。"""

    def __init__(self, code: str, message: str | None = None) -> None:
        self.code = code
        text = message or ERROR_MESSAGES.get(code, code)
        super().__init__(f"{code}: {text}")


class BudgetExceeded(ReplayError):
    def __init__(self, message: str) -> None:
        super().__init__(E_BUDGET_EXCEEDED, message)


class RecoveryInvalid(ReplayError):
    def __init__(self, message: str) -> None:
        super().__init__(E_RECOVERY_INVALID, message)


class CancelledError(ReplayError):
    def __init__(self, message: str = "任务已取消") -> None:
        super().__init__(E_CANCELLED, message)
