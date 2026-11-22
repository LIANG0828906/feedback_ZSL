"""统一错误码与异常类型。"""

E_SCHEMA = "E_SCHEMA"  # 字段缺失 / 结构非法
E_REF = "E_REF"        # 引用悬空 / 跨段引用
E_PARA = "E_PARA"      # 参数数值非法
E_FIT = "E_FIT"        # 排版与可读时长冲突无法收敛
E_STATE = "E_STATE"    # 非法状态迁移
E_SEG = "E_SEG"        # 坏语音段（可重试，不污染其他台词）


class PipelineError(Exception):
    """携带错误码、字段名与明细的业务异常。"""

    def __init__(self, code, message="", field=None, details=None):
        self.code = code
        self.field = field
        self.details = details or {}
        msg = f"[{code}] {message}"
        if field:
            msg += f" (field: {field})"
        super().__init__(msg)

    def to_dict(self):
        return {"code": self.code, "field": self.field,
                "message": str(self), "details": self.details}
