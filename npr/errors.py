"""Pipeline error codes.

Every user-facing failure uses one of the ``E_*`` codes defined here.  The
short message always carries the first offending location (path number, layer,
version, ...) so failures are locatable.
"""

E_SCENE_INVALID = "E_SCENE_INVALID"
E_COMMIT_FAILED = "E_COMMIT_FAILED"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_STALE_VERSION = "E_STALE_VERSION"
E_BUSY = "E_BUSY"
E_CANCELLED = "E_CANCELLED"


class PipelineError(Exception):
    def __init__(self, code, message):
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message

    @classmethod
    def scene_invalid(cls, detail, path_index=None):
        where = "" if path_index is None else f" path#{path_index}"
        return cls(E_SCENE_INVALID, f"{detail}{where}")

    @classmethod
    def commit_failed(cls, detail):
        return cls(E_COMMIT_FAILED, detail)

    @classmethod
    def recovery_invalid(cls, detail, layer=None):
        where = "" if layer is None else f" first broken layer={layer}"
        return cls(E_RECOVERY_INVALID, f"{detail}{where}")

    @classmethod
    def stale_version(cls, detail):
        return cls(E_STALE_VERSION, detail)

    @classmethod
    def busy(cls, detail="a recompute task is already running"):
        return cls(E_BUSY, detail)
