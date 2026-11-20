"""统一错误码。

结算期可预期的拒绝（迟到 / 超额 / 重复 / 非法）以事件形式记录在帧日志中；
只有运行级失败（快照写、恢复校验、基线版本、并发冲突、取消）才抛异常。
"""


class ReplayError(Exception):
    code = "E_REPLAY"

    def __init__(self, message="", **details):
        super().__init__(message or self.code)
        self.details = details

    def to_dict(self):
        return {"error": self.code, "message": str(self), **self.details}


class SnapshotWriteError(ReplayError):
    code = "E_SNAPSHOT_WRITE"

    def __init__(self, step, reason):
        super().__init__(f"snapshot write failed at step {step}: {reason}",
                         step=step, reason=str(reason))


class RecoveryInvalidError(ReplayError):
    code = "E_RECOVERY_INVALID"

    def __init__(self, reason, step=None):
        super().__init__(f"recovery invalid at step {step}: {reason}",
                         reason=reason, step=step)


class BaselineVersionError(ReplayError):
    code = "E_BASELINE_VERSION"

    def __init__(self, expected, actual):
        super().__init__(
            f"baseline version mismatch: expected {expected}, got {actual}",
            expected=expected, actual=actual)


class BusyError(ReplayError):
    code = "E_BUSY"

    def __init__(self, match_id):
        super().__init__(
            f"match {match_id} already has a running replay task",
            match_id=match_id)


class CancelledError(ReplayError):
    code = "E_CANCELLED"

    def __init__(self, step):
        super().__init__(f"replay cancelled at step {step}", step=step)
