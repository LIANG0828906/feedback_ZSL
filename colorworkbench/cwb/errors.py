"""工作台统一错误码与异常类型。

所有面向业务的失败都通过 :class:`CwbError` 抛出并携带稳定的错误码，
便于测试与界面异常链路定位。
"""

from __future__ import annotations


# 错误码常量（与技术说明、测试保持一致）
E_VER = "E_VER"          # 未知配置版本号 / 必填标签缺失
E_PARA = "E_PARA"        # 标签值越界 / 参数非法
E_DEPTH = "E_DEPTH"      # 位深声明与实际数据不符
E_WP = "E_WP"            # 白点缺失且无声明默认值
E_INTENT = "E_INTENT"    # 渲染意图枚举不受支持
E_DECODE = "E_DECODE"    # 图像损坏无法解码
E_DIFF = "E_DIFF"        # 增量与全量结果逐像素不一致
E_SNAPSHOT = "E_SNAPSHOT"  # 快照版本/哈希校验失败
E_BATCH = "E_BATCH"      # 单批转换内部失败
E_BUSY = "E_BUSY"        # 资源缓冲耗尽（内部以阻塞处理，不对外静默丢弃）


class CwbError(Exception):
    """带稳定错误码的工作台异常。"""

    def __init__(self, code: str, message: str, tag: str | None = None,
                 detail: dict | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        # 越界参数标签名（E_PARA 必须指出标签名）
        self.tag = tag
        self.detail = detail or {}

    def __str__(self) -> str:  # pragma: no cover - 调试辅助
        suffix = f" [{self.tag}]" if self.tag else ""
        return f"{self.code}: {self.message}{suffix}"
