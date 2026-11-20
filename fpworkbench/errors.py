"""Business error codes used across the workbench."""

E_FMT_INVALID = "E_FMT_INVALID"
E_MIGRATE_LOSS = "E_MIGRATE_LOSS"
E_BASELINE_MISMATCH = "E_BASELINE_MISMATCH"
E_BUSY = "E_BUSY"
E_LIB_MISMATCH = "E_LIB_MISMATCH"


class FpError(Exception):
    """Error carrying a business code plus structured detail."""

    def __init__(self, code, message, *, file=None, field=None, detail=None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.file = file
        self.field = field
        self.detail = detail or {}

    def to_dict(self):
        out = {"code": self.code, "message": self.message}
        if self.file is not None:
            out["file"] = self.file
        if self.field is not None:
            out["field"] = self.field
        out.update(self.detail)
        return out

    def __str__(self):
        loc = ""
        if self.file:
            loc = f" file={self.file}"
            if self.field:
                loc += f" field={self.field}"
        return f"[{self.code}] {self.message}{loc}"
