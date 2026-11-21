"""Business error codes for the retargeting pipeline."""

E_ANIM_INVALID = "E_ANIM_INVALID"
E_MAP_CONFLICT = "E_MAP_CONFLICT"
E_ERROR_ATTR_UNKNOWN = "E_ERROR_ATTR_UNKNOWN"
E_SCALE_APPLIED = "E_SCALE_APPLIED"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_RUN_INCOMPLETE = "E_RUN_INCOMPLETE"


class RetargetError(Exception):
    """Pipeline error carrying a business code and structured details."""

    def __init__(self, code, message, details=None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details or {}

    def to_dict(self):
        return {"error": self.code, "message": self.message, "details": self.details}
