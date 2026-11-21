"""Factor-based invalidation, incremental recompute and trace logging."""

import os
import threading

from . import LAYER_OUTLINE, LAYER_SHADOW, LAYER_SMEAR
from .canonical import canonical_bytes, canonical_hash, write_json_atomic
from .errors import E_CANCELLED, PipelineError
from .params import factor_hash
from .storage import Store
from .strokes import generate_outline, generate_shadow, generate_smear, remove_culled

TRACE_NAME = "trace.jsonl"


def plan_invalidation(old_factors, new_factors):
    """Return the set of layers that must be recomputed.

    Dependency graph: outline -> shadow (culling closure) -> smear (density).
    Any factor whose hash changes invalidates that layer and every layer
    downstream of it; unknown changes invalidate everything (conservative).
    """
    affected = set()
    if old_factors is None:
        return {LAYER_OUTLINE, LAYER_SHADOW, LAYER_SMEAR}, "initial build"
    if set(old_factors) != set(new_factors):
        return {LAYER_OUTLINE, LAYER_SHADOW, LAYER_SMEAR}, "factor set changed"

    reasons = []
    if old_factors["scene"] != new_factors["scene"]:
        affected.update((LAYER_OUTLINE, LAYER_SHADOW, LAYER_SMEAR))
        reasons.append("scene geometry changed -> all layers")
    if old_factors["outline"] != new_factors["outline"]:
        affected.update((LAYER_OUTLINE, LAYER_SHADOW, LAYER_SMEAR))
        reasons.append("outline factor changed -> outline + culling closure + smear")
    if old_factors["shadow"] != new_factors["shadow"]:
        affected.update((LAYER_SHADOW, LAYER_SMEAR))
        reasons.append("shadow factor changed -> shadow + density closure")
    if old_factors["smear"] != new_factors["smear"]:
        affected.add(LAYER_SMEAR)
        reasons.append("smear factor changed -> smear")
    if not affected:
        reasons.append("no factor change")
    return affected, "; ".join(reasons)


class Pipeline:
    def __init__(self, workdir):
        self.workdir = os.fspath(workdir)
        os.makedirs(self.workdir, exist_ok=True)
        self.store = Store(os.path.join(self.workdir, "store"))
        self.trace_path = os.path.join(self.workdir, TRACE_NAME)
        self._task_lock = threading.Lock()
        self._cancel = threading.Event()
        self._running = False
        # Deterministic crash injection for kill/recovery verification:
        # {"layer": int, "version": int} makes the process exit hard at that
        # boundary, exactly like SIGKILL (no cleanup, no exit report).
        self.crash_hook = None

    # ------------------------------------------------------------- factors
    @staticmethod
    def build_factors(scene, outline_p, shadow_p, smear_p):
        return {
            "scene": scene.scene_hash,
            "outline": factor_hash(outline_p.factor()),
            "shadow": factor_hash(shadow_p.factor()),
            "smear": factor_hash(smear_p.factor()),
        }

    def save_params(self, scene, outline_p, shadow_p, smear_p):
        state = {
            "scene_hash": scene.scene_hash,
            "params": {
                "outline": outline_p.factor(),
                "shadow": shadow_p.factor(),
                "smear": smear_p.factor(),
            },
        }
        write_json_atomic(os.path.join(self.workdir, "params.json"), state)

    # ------------------------------------------------------------- recompute
    def recompute(self, scene, outline_p, shadow_p, smear_p, cancel_event=None):
        """One recompute task.  Raises E_BUSY if another task is active."""
        if not self._task_lock.acquire(blocking=False):
            raise PipelineError.busy()
        self._cancel = cancel_event or threading.Event()
        try:
            self._running = True
            return self._recompute_locked(scene, outline_p, shadow_p, smear_p)
        finally:
            self._running = False
            self._task_lock.release()

    def is_running(self):
        return self._running

    def _recompute_locked(self, scene, outline_p, shadow_p, smear_p):
        old_manifest = self.store.recover()
        factors = self.build_factors(scene, outline_p, shadow_p, smear_p)
        old_factors = None if old_manifest is None else old_manifest["factors"]
        affected, reason = plan_invalidation(old_factors, factors)
        version = 1 if old_manifest is None else old_manifest["version"] + 1
        self._active_version = version
        self._active_layer = LAYER_OUTLINE

        layer_outputs, reused_entries, reused_batches = self._reuse_entries(
            old_manifest, affected
        )
        all_strokes = {}

        if LAYER_OUTLINE in affected:
            outline_strokes = generate_outline(scene, outline_p)
            layer_outputs[LAYER_OUTLINE] = {
                "strokes": outline_strokes,
                "culled": [],
                "factor_hash": factors["outline"],
            }
            all_strokes[LAYER_OUTLINE] = outline_strokes
        else:
            all_strokes[LAYER_OUTLINE] = self.store.read_layer_strokes(
                old_manifest["version"], LAYER_OUTLINE
            )
        self._active_layer = LAYER_OUTLINE
        self._checkpoint()

        if LAYER_SHADOW in affected:
            shadow_all, culled = generate_shadow(
                scene, shadow_p, all_strokes[LAYER_OUTLINE]
            )
            shadow_strokes = remove_culled(shadow_all, culled)
            layer_outputs[LAYER_SHADOW] = {
                "strokes": shadow_strokes,
                "culled": culled,
                "factor_hash": factors["shadow"],
            }
            all_strokes[LAYER_SHADOW] = shadow_strokes
        else:
            all_strokes[LAYER_SHADOW] = self.store.read_layer_strokes(
                old_manifest["version"], LAYER_SHADOW
            )
        self._active_layer = LAYER_SHADOW
        self._checkpoint()

        if LAYER_SMEAR in affected:
            smear_strokes = generate_smear(
                scene, smear_p, all_strokes[LAYER_SHADOW]
            )
            layer_outputs[LAYER_SMEAR] = {
                "strokes": smear_strokes,
                "culled": [],
                "factor_hash": factors["smear"],
            }
        self._active_layer = LAYER_SMEAR
        self._checkpoint()

        if old_manifest is not None:
            self.store.snapshot_historical_manifest(old_manifest)
        manifest = self.store.commit_version(
            version,
            layer_outputs,
            factors,
            old_manifest,
            crash_hook=self._commit_hook(),
            cancel_check=lambda layer, batch_id: self._checkpoint(),
        )
        self.save_params(scene, outline_p, shadow_p, smear_p)

        stats = self._trace(
            manifest, layer_outputs, reused_entries, reused_batches, affected, reason
        )
        return manifest, stats

    def _checkpoint(self):
        """Cooperative cancellation is honored at layer/batch boundaries."""
        if self._cancel.is_set():
            raise PipelineError(E_CANCELLED, "recompute cancelled at batch boundary")

    def _commit_hook(self):
        if self.crash_hook is None:
            return None
        hook = self.crash_hook
        if hook.get("version") != self._active_version:
            return None

        def fire(layer, batch_id):
            if hook.get("layer") == layer and hook.get("batch", batch_id) == batch_id:
                os._exit(9)

        return fire

    def _reuse_entries(self, old_manifest, affected):
        outputs = {}
        reused_entries = []
        reused_batches = set()
        if old_manifest is None:
            return outputs, reused_entries, reused_batches
        for entry in old_manifest["entries"]:
            if entry["layer"] not in affected:
                reused_entries.append(entry)
                reused_batches.add((entry["layer"], entry["batch"]))
        return outputs, reused_entries, reused_batches

    def _trace(self, manifest, layer_outputs, reused_entries, reused_batches, affected, reason):
        stats = []
        for layer in (LAYER_OUTLINE, LAYER_SHADOW, LAYER_SMEAR):
            new_entries = [e for e in manifest["entries"] if e["layer"] == layer]
            new_batches = {(e["layer"], e["batch"]) for e in new_entries}
            reused_layer = sum(1 for (l, _) in reused_batches if l == layer)
            culled = manifest["culled"].get(str(layer), [])
            hashes = sorted(e["content_hash"] for e in new_entries)
            record = {
                "version": manifest["version"],
                "layer": layer,
                "affected": layer in affected,
                "reason": reason,
                "recomputed_batches": len(new_batches),
                "reused_batches": reused_layer,
                "chunk_count": len(new_entries),
                "culled_count": len(culled),
                "culled_ids": culled,
                "layer_hash": canonical_hash(hashes),
            }
            stats.append(record)
            with open(self.trace_path, "a", encoding="utf-8", newline="\n") as fh:
                fh.write(canonical_hash(record)[:16])
                fh.write(" ")
                fh.write(canonical_bytes(record).decode("utf-8"))
                fh.write("\n")
        return stats

    # ------------------------------------------------------------- recovery
    def recover(self):
        manifest = self.store.recover()
        if manifest is None:
            return None, self._missing_layers(set())
        present_layers = {e["layer"] for e in manifest["entries"]}
        missing = self._missing_layers(present_layers)
        return manifest, missing

    @staticmethod
    def _missing_layers(present):
        return sorted({LAYER_OUTLINE, LAYER_SHADOW, LAYER_SMEAR} - present)

    def resume(self, scene, outline_p, shadow_p, smear_p):
        """Continue an incomplete committed version; no-op when complete."""
        manifest, missing = self.recover()
        if manifest is None or not missing:
            return manifest, []
        return self.recompute(scene, outline_p, shadow_p, smear_p)
