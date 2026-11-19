class OobError(Exception):
    """Base exception carrying a stable business error code."""

    code = "E_OOB"
    http_status = 400

    def __init__(self, message: str, *, details=None):
        super().__init__(message)
        self.message = message
        self.details = details

    def as_dict(self) -> dict:
        result = {"code": self.code, "message": self.message}
        if self.details is not None:
            result["details"] = self.details
        return result


class ConfigError(OobError):
    code = "E_CONFIG"
    http_status = 400


class ThresholdConfigError(ConfigError):
    code = "E_THRESHOLD_INVALID"
    http_status = 409


class ConfigSchemaError(ConfigError):
    code = "E_CONFIG_SCHEMA"
    http_status = 400


class ConfigReadError(ConfigError):
    code = "E_CONFIG_READ"
    http_status = 400


class ConfigImmutableError(ConfigError):
    code = "E_CONFIG_IMMUTABLE"
    http_status = 409


class PortInUseError(OobError):
    code = "E_PORT_IN_USE"
    http_status = 409


class CollectionError(OobError):
    code = "E_COLLECTION"
    http_status = 502


class ProtocolError(CollectionError):
    """A protocol-level transport or response failure."""

    def __init__(self, protocol: str, message: str, *, details=None):
        super().__init__(f"{protocol}: {message}", details=details)
        self.protocol = protocol
        self.reason = message


class UnparsableError(CollectionError):
    code = "E_UNPARSABLE"
    http_status = 422
