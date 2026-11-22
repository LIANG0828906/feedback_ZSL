"""Error codes and the structured pipeline exception."""

E_SCHEMA = "E_SCHEMA"
E_REF = "E_REF"
E_WIN = "E_WIN"
E_GAP = "E_GAP"
E_PARA = "E_PARA"
E_TIMING = "E_TIMING"
E_STATE = "E_STATE"
E_DECODE = "E_DECODE"


class PipelineError(Exception):
    """Structured error carrying an error code and diagnostic context.

    ``subtitle_id`` is the 1-based dialogue number used by the business
    specification; ``field`` names the offending manifest field when known.
    """

    def __init__(self, code, message, subtitle_id=None, field=None, details=None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.subtitle_id = subtitle_id
        self.field = field
        self.details = details or {}

    def to_dict(self):
        return {
            "code": self.code,
            "message": self.message,
            "subtitle_id": self.subtitle_id,
            "field": self.field,
            "details": self.details,
        }

    def __str__(self):
        loc = []
        if self.subtitle_id is not None:
            loc.append(f"subtitle#{self.subtitle_id}")
        if self.field:
            loc.append(f"field={self.field}")
        prefix = f"[{self.code}] " + (" ".join(loc) + " " if loc else "")
        return f"{prefix}{self.message}"
