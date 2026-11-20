"""时域去噪门限与直达声保护窗口。"""

from __future__ import annotations

import numpy as np

from .config import EnhanceConfig
from .errors import OnsetProtectedError


def protection_window(onset: int, n_samples: int, pre: int, post: int) -> tuple:
    start = max(0, onset - pre)
    end = min(n_samples, onset + post)
    return start, end


def apply_gate(
    x: np.ndarray,
    residual_rms: float,
    onset: int,
    config: EnhanceConfig,
    channel: int,
) -> np.ndarray:
    """对保护窗口之外的样本应用时域门限，窗口内样本保持谱域结果不变。"""
    gp = config.gate_for(channel)
    pp = config.protect_for(channel)
    n = x.shape[0]
    thr = gp.threshold_ratio * max(residual_rms, 1e-12)
    out = x.copy()
    w_start, w_end = protection_window(onset, n, pp.pre, pp.post)
    mask = np.abs(out) < thr
    mask[w_start:w_end] = False
    if gp.mode == "attenuate":
        out[mask] *= gp.attenuation
    else:
        out[mask] = 0.0

    delta = np.abs(out[w_start:w_end] - x[w_start:w_end])
    if delta.size and float(delta.max()) > pp.modify_tol:
        idx = int(np.argmax(delta)) + w_start
        raise OnsetProtectedError(
            "保护窗口内样本被门限修改",
            channel=channel,
            sample=idx,
            delta=float(delta.max()),
            tolerance=pp.modify_tol,
        )
    return out
