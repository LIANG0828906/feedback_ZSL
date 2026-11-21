"""赛事直播本地素材审看：确定性切片、参数版本、依赖图谱与断点恢复。

包内所有模块均为本地确定性实现，不依赖网络与真实编解码库。
"""

from .errors import AuditError, ErrorCode

__all__ = ["AuditError", "ErrorCode"]
__version__ = "1.0.0"
