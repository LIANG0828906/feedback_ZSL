"""Hand-drawn style non-photorealistic rendering pipeline.

Layers (fixed layer numbers):
  0 = outline strokes
  1 = hatching shadow strokes
  2 = color smear strokes

All generated artifacts are deterministic for identical inputs: coordinates are
quantized to a fixed decimal grid, serialization has a canonical form, randomness
derives from an explicit seeded PCG64 stream, and no wall-clock time, PID or
file size participates in any hash or log truth value.
"""

LAYER_OUTLINE = 0
LAYER_SHADOW = 1
LAYER_SMEAR = 2
LAYER_NAMES = ("outline", "shadow", "smear")

QUANT_DECIMALS = 6


def qround(value):
    """Round a scalar or array onto the fixed deterministic coordinate grid."""
    import numpy as np

    return np.round(value, QUANT_DECIMALS)
