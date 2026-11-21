"""统一错误码。

所有业务异常都带稳定的字符串错误码，便于脚本化实验与测试断言；
异常同时承载定位信息（首个异常帧号 / 断链帧号 / 候选因子等）。
"""

E_HDR_INVALID = "E_HDR_INVALID"
E_MODE_INVALID = "E_MODE_INVALID"
E_ERROR_UNBOUNDED = "E_ERROR_UNBOUNDED"
E_VERSION_STALE = "E_VERSION_STALE"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"


class HDRError(Exception):
    """所有工作台异常的基类。"""

    code = "E_UNKNOWN"

    def __init__(self, message, **details):
        super().__init__(message)
        self.message = message
        self.details = details

    def to_dict(self):
        data = {"code": self.code, "message": self.message}
        data.update(self.details)
        return data


class HDRInvalidError(HDRError):
    """头部非法、传递函数未知或帧数据截断。"""

    code = E_HDR_INVALID

    def __init__(self, message, bad_frame=None, reason=None):
        super().__init__(message, bad_frame=bad_frame, reason=reason)


class ModeInvalidError(HDRError):
    """转换模式非法或未初始化，且不得产出任何帧。"""

    code = E_MODE_INVALID

    def __init__(self, message, mode=None, reason=None):
        super().__init__(message, mode=mode, reason=reason)


class ErrorUnboundedError(HDRError):
    """误差无法归因到有界环节。"""

    code = E_ERROR_UNBOUNDED

    def __init__(self, message, candidates=None, bad_frame=None):
        super().__init__(
            message, candidates=list(candidates or ()), bad_frame=bad_frame
        )


class VersionStaleError(HDRError):
    """版本哈希与发布清单不一致，查询必须拒绝并返回空结果。"""

    code = E_VERSION_STALE

    def __init__(self, message, version=None):
        super().__init__(message, version=version)


class RecoveryInvalidError(HDRError):
    """快照断链或字段缺失。"""

    code = E_RECOVERY_INVALID

    def __init__(self, message, bad_frame=None, reason=None):
        super().__init__(message, bad_frame=bad_frame, reason=reason)

