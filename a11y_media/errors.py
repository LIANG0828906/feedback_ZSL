"""统一错误码与组件异常。

错误码约定:
- E_SCHEMA  : 清单字段缺失 / 引用越界 / 结构非法
- E_PARA    : 参数越界(语速、权重、帧长等), 附带字段名
- E_ERR     : 实测误差超出理论上界, 附带句号与误差项名称
- E_STATE   : 分段状态机非法迁移, 原状态保持不变
- E_DIVERGE : 实时 / 断点恢复 / 全量重算三条路径结果不一致
"""

from __future__ import annotations


E_SCHEMA = "E_SCHEMA"
E_PARA = "E_PARA"
E_ERR = "E_ERR"
E_STATE = "E_STATE"
E_DIVERGE = "E_DIVERGE"


class ComponentError(Exception):
    """领域组件统一异常。code 为上述错误码, context 携带定位信息。"""

    def __init__(self, code: str, message: str, **context):
        super().__init__(f"[{code}] {message} | {context}" if context else f"[{code}] {message}")
        self.code = code
        self.message = message
        self.context = context

    def __repr__(self) -> str:  # pragma: no cover
        return f"ComponentError(code={self.code!r}, message={self.message!r}, context={self.context!r})"


def schema_error(message: str, **context) -> ComponentError:
    return ComponentError(E_SCHEMA, message, **context)


def para_error(message: str, **context) -> ComponentError:
    return ComponentError(E_PARA, message, **context)


def err_error(message: str, **context) -> ComponentError:
    return ComponentError(E_ERR, message, **context)


def state_error(message: str, **context) -> ComponentError:
    return ComponentError(E_STATE, message, **context)


def diverge_error(message: str, **context) -> ComponentError:
    return ComponentError(E_DIVERGE, message, **context)
