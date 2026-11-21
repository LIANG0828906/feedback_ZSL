"""Memory budget metering and the declared degradation sequence."""

from .errors import BudgetExceeded
from .fingerprint import frame_count

FLOAT_BYTES = 8          # float64 magnitude bin
PEAK_REC_BYTES = 32      # one peak: (t, f, band) record
HASH_ENTRY_BYTES = 48    # one temp hash-table entry
BASE_OVERHEAD = 64 * 1024


def stage_params(fp_cfg, budget_cfg, stage):
    """Buffering parameters for a degradation stage (0 = full fidelity)."""
    params = {"frame_batch": fp_cfg["frame_batch"], "hash_chunk": 0}
    for step in budget_cfg.get("degrade_sequence", []):
        if step["stage"] <= stage:
            if "frame_batch" in step:
                params["frame_batch"] = step["frame_batch"]
            if "hash_chunk" in step:
                params["hash_chunk"] = step["hash_chunk"]
    return params


def max_stage(budget_cfg):
    stages = [s["stage"] for s in budget_cfg.get("degrade_sequence", [])]
    return max([0] + stages)


def estimate_memory(n_frames, n_fft, n_peaks, n_hashes, frame_batch, hash_chunk=0):
    """Metered bytes: spectrum buffer + peak set + hash table temp + base."""
    spectrum = frame_batch * (n_fft // 2 + 1) * FLOAT_BYTES
    peaks = n_peaks * PEAK_REC_BYTES
    hash_rows = min(n_hashes, hash_chunk) if hash_chunk else n_hashes
    hash_table = hash_rows * HASH_ENTRY_BYTES
    return BASE_OVERHEAD + spectrum + peaks + hash_table


def estimate_task(n_samples, rate, fp_cfg, budget_cfg, stage):
    """Deterministic pre-start estimate (upper bound) for a sample task."""
    n_frames = frame_count(n_samples, fp_cfg["frame_size"], fp_cfg["hop_size"])
    n_peaks = n_frames * len(fp_cfg["bands"]) * fp_cfg["peaks_per_band"]
    n_hashes = n_peaks * fp_cfg["fan_out"]
    params = stage_params(fp_cfg, budget_cfg, stage)
    mem = estimate_memory(
        n_frames, fp_cfg["frame_size"], n_peaks, n_hashes,
        params["frame_batch"], params["hash_chunk"],
    )
    return mem, params


def actual_usage(stats, fp_cfg, params):
    """Metered usage after a run, from actual peak/hash counts."""
    return estimate_memory(
        stats["n_frames"], fp_cfg["frame_size"],
        stats["n_peaks"], stats["n_hashes"],
        params["frame_batch"], params["hash_chunk"],
    )


def resolve_stage(n_samples, rate, fp_cfg, budget_cfg, per_task_budget):
    """Pick the first declared stage whose estimate fits the budget.

    Returns (stage, estimate, params, attempts). Raises BudgetExceeded
    (E_BUDGET_EXCEEDED) when every declared stage still exceeds the budget.
    """
    attempts = []
    for stage in range(max_stage(budget_cfg) + 1):
        est, params = estimate_task(n_samples, rate, fp_cfg, budget_cfg, stage)
        attempts.append({"stage": stage, "estimate": est})
        if est <= per_task_budget:
            return stage, est, params, attempts
    raise BudgetExceeded(
        "all degradation stages exceed per-task memory budget",
        attempts=attempts, budget=per_task_budget,
    )
