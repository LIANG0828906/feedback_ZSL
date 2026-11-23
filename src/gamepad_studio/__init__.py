"""PadWeave Studio deterministic gamepad mapping engine."""

from .curves import apply_curve, normalize_axis
from .errors import (
    ALGORITHM_VERSION,
    AxisError,
    DivergenceError,
    MappingError,
    OrderError,
    ParameterError,
    SchemaError,
    StudioError,
)
from .models import (
    ActionSample,
    ActionSnapshot,
    AxisSample,
    AxisSpec,
    BindingRule,
    BindingTable,
    CurveSpec,
    DeviceSpec,
    InputEvent,
    SampleBundle,
    TriggerSample,
    TruthManifest,
)

__all__ = [
    "ALGORITHM_VERSION",
    "ActionSample",
    "ActionSnapshot",
    "AxisError",
    "AxisSample",
    "AxisSpec",
    "BindingRule",
    "BindingTable",
    "CurveSpec",
    "DeviceSpec",
    "DivergenceError",
    "InputEvent",
    "MappingError",
    "OrderError",
    "ParameterError",
    "SampleBundle",
    "SchemaError",
    "StudioError",
    "TriggerSample",
    "TruthManifest",
    "apply_curve",
    "normalize_axis",
]

