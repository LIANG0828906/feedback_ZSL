class DualWriteError(Exception):
    code = "E_DUALWRITE"


class ConnectionLimitError(DualWriteError):
    code = "E_CONN"


class ConfigurationError(DualWriteError):
    code = "E_CONFIG"


class ValidationBusyError(DualWriteError):
    code = "E_VALIDATION_BUSY"

