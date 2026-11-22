"""Forensics workbench error codes and the single exception type used everywhere.

All recoverable, expected failures in the pipeline raise :class:`ForensicsError`
carrying one of the stable ``E_*`` codes required by the specification.
Unexpected programming errors are allowed to propagate as normal exceptions.
"""

# Image / manifest cannot be read or decoded, or manifest file is missing.
E_DECODE = "E_DECODE"
# Image metadata (resolution / channel count) disagrees with the manifest.
E_META = "E_META"
# A configurable parameter is outside its legal domain.
E_PARA = "E_PARA"
# Preview and precise paths disagree beyond the declared tolerance.
E_DIVERGE = "E_DIVERGE"
# A result/cache block is used with a parameter or algorithm version that does
# not match the snapshot it was produced under.
E_VER = "E_VER"
# An illegal block state-machine transition was requested.
E_STATE = "E_STATE"


ERROR_CODES = (E_DECODE, E_META, E_PARA, E_DIVERGE, E_VER, E_STATE)


class ForensicsError(Exception):
    """Raised for every defined, business-level failure.

    ``code`` is one of the ``E_*`` constants; ``field`` optionally names the
    offending parameter or metadata field (required for E_META / E_PARA);
    ``block_id`` and ``metric`` carry the first-diverging-block / metric
    information required by E_DIVERGE.
    """

    def __init__(self, code, message, field=None, block_id=None, metric=None):
        super().__init__(message)
        if code not in ERROR_CODES:
            raise ValueError("unknown error code: %r" % (code,))
        self.code = code
        self.message = message
        self.field = field
        self.block_id = block_id
        self.metric = metric

    def __str__(self):
        parts = [self.code, self.message]
        if self.field is not None:
            parts.append("field=%s" % self.field)
        if self.block_id is not None:
            parts.append("block_id=%s" % self.block_id)
        if self.metric is not None:
            parts.append("metric=%s" % self.metric)
        return " | ".join(parts)

    def to_dict(self):
        return {
            "code": self.code,
            "message": self.message,
            "field": self.field,
            "block_id": self.block_id,
            "metric": self.metric,
        }
