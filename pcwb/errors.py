"""统一错误码。所有业务失败均以带 code 的异常抛出，便于脚本化校验。"""


class PcwbError(Exception):
    code = "E_UNKNOWN"

    def __init__(self, message, **detail):
        super().__init__(message)
        self.message = message
        self.detail = detail

    def __str__(self):
        if self.detail:
            kv = " ".join(f"{k}={v!r}" for k, v in sorted(self.detail.items()))
            return f"{self.code}: {self.message} ({kv})"
        return f"{self.code}: {self.message}"


class PointcloudInvalid(PcwbError):
    """格式错误或坐标越界。detail 中给出 field（头部字段）或 point_index。"""

    code = "E_POINTCLOUD_INVALID"


class LevelBudget(PcwbError):
    """显示选择超出预算。detail.usage 给出各块预算用量。"""

    code = "E_LEVEL_BUDGET"


class BlockSeam(PcwbError):
    """分块显示与单块真值不一致。detail.block_pair 定位边界块对。"""

    code = "E_BLOCK_SEAM"


class OpInvalid(PcwbError):
    """选区为空、参数非法或输出点数超上限。"""

    code = "E_OP_INVALID"


class CommitFailed(PcwbError):
    """原子提交失败，上一版本保持不变。"""

    code = "E_COMMIT_FAILED"


class StaleHandle(PcwbError):
    """渲染线程读到了已废弃的快照句柄。"""

    code = "E_STALE_HANDLE"
