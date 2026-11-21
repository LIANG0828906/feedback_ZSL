"""Material library engine: incremental recompute, atomic publish,
concurrent snapshot readers, cancellation and trace logging.

One recompute task per library at a time (duplicate submissions fail with
E_BUSY).  Cancellation is honored at per-version checkpoints and keeps the
last published release.  Readers acquire snapshot handles to immutable
published releases; using a released handle fails with E_STALE_SNAPSHOT.
"""

from __future__ import annotations

import itertools
import threading

from . import persistence
from .canon import canon, content_hash
from .errors import ErrorCode, SpectralStudioError
from .pipeline import PipelineEnvironment, SharedCaches, run_chain
from .spectrum import import_material
from .versioning import (GENESIS_HASH, MaterialVersion, Release,
                         STATUS_INCOMPLETE, STATUS_PUBLISHED)
from . import colorimetry as clr


class SnapshotHandle:
    """Reader handle pinning one immutable published release."""

    def __init__(self, handle_id, release):
        self.handle_id = handle_id
        self.release = release
        self.active = True


class MaterialLibrary:
    def __init__(self, env=None, snapshot_path=None):
        self.env = env or PipelineEnvironment()
        self.snapshot_path = snapshot_path
        self.caches = SharedCaches()
        self._lock = threading.RLock()
        self._material_params = {}   # material_id -> param dict (source doc)
        self._specs = {}             # material_id -> MaterialSpec
        self._history = {}           # material_id -> [MaterialVersion]
        self._release = None
        self._trace = []
        self._trace_seq = 0
        self._busy = False
        self._cancel_event = threading.Event()
        self._handles = {}
        self._handle_seq = itertools.count(1)
        self.ui_state = {}
        # Testing hook: artificial per-version delay so forced-kill
        # scenarios can be exercised deterministically.
        self.recompute_delay = 0.0

    # ------------------------------------------------------------------
    # material registration
    # ------------------------------------------------------------------
    def _import_spec(self, params):
        ref_ill = clr.illuminant_spd("D65")
        ref_cmf = clr.color_matching_functions(clr.OBSERVER_1931_2)
        return import_material(params, ref_ill, ref_cmf)

    def add_material(self, doc):
        spec = self._import_spec(doc)
        with self._lock:
            self._material_params[spec.material_id] = canon(doc)
            self._specs[spec.material_id] = spec
        return spec.material_id

    # ------------------------------------------------------------------
    # snapshot handles for concurrent readers
    # ------------------------------------------------------------------
    def acquire_snapshot(self):
        with self._lock:
            if self._release is None:
                raise SpectralStudioError(ErrorCode.E_STALE_SNAPSHOT,
                                          "no release published yet")
            handle = SnapshotHandle(next(self._handle_seq), self._release)
            self._handles[handle.handle_id] = handle
            return handle

    def read_material(self, handle, material_id):
        with self._lock:
            if not isinstance(handle, SnapshotHandle) or not handle.active \
                    or self._handles.get(handle.handle_id) is not handle:
                raise SpectralStudioError(
                    ErrorCode.E_STALE_SNAPSHOT,
                    "snapshot handle is retired; re-acquire a snapshot")
            version = handle.release.versions.get(material_id)
            if version is None:
                raise KeyError(material_id)
            return {
                "material_id": material_id,
                "version": version.version,
                "encoded": dict(version.encoded),
                "content_hash": version.content_hash,
                "release_id": handle.release.release_id,
            }

    def release_snapshot(self, handle):
        with self._lock:
            handle.active = False
            self._handles.pop(handle.handle_id, None)

    # ------------------------------------------------------------------
    # tracing
    # ------------------------------------------------------------------
    def _append_trace(self, entry):
        self._trace_seq += 1
        entry = dict(entry)
        entry["seq"] = self._trace_seq
        self._trace.append(canon(entry))
        return entry

    @property
    def trace(self):
        return list(self._trace)

    # ------------------------------------------------------------------
    # recompute + publish
    # ------------------------------------------------------------------
    def cancel(self):
        self._cancel_event.set()

    @property
    def current_release(self):
        with self._lock:
            return self._release

    def history_of(self, material_id):
        return list(self._history.get(material_id, []))

    def submit(self, ops, ui_state=None):
        """Synchronous submit; raises E_BUSY if a recompute is running."""
        with self._lock:
            if self._busy:
                raise SpectralStudioError(ErrorCode.E_BUSY,
                                          "a recompute task is already running")
            self._busy = True
            self._cancel_event.clear()
        try:
            return self._apply(ops, ui_state)
        finally:
            with self._lock:
                self._busy = False

    def _apply(self, ops, ui_state):
        ops = list(ops)
        with self._lock:
            old_release = self._release
            old_env = self.env

        # 1. Apply environment ops.
        env_config = old_env.to_config()
        material_edits = []  # list of (material_id or None, params)
        rollbacks = []
        for op in ops:
            kind = op.get("op")
            if kind == "set_env":
                for key in ("illuminant", "white_point", "observer",
                            "adaptation", "targets"):
                    if key in op:
                        env_config[key] = op[key]
            elif kind == "set_material":
                material_edits.append((op["id"], op["params"]))
            elif kind == "add_material":
                material_edits.append((None, op["doc"]))
            elif kind == "rollback":
                rollbacks.append((op["id"], int(op["version"])))
            else:
                raise SpectralStudioError(ErrorCode.E_COLOR_INVALID,
                                          "unknown op", {"op": kind})
        new_env = PipelineEnvironment.from_config(env_config)
        new_dep = new_env.dependency_factors()

        # 2. Determine the affected version set.
        affected = {}  # material_id -> reason
        for edit_mid, params in material_edits:
            if edit_mid is None:
                mid = self.add_material(params)
            else:
                mid = edit_mid
                spec = self._import_spec(params)
                with self._lock:
                    self._material_params[mid] = canon(params)
                    self._specs[mid] = spec
            affected[mid] = "material-params-changed"
        for mid, target_version in rollbacks:
            history = self._history.get(mid, [])
            match = [v for v in history if v.version == target_version]
            if not match:
                raise SpectralStudioError(
                    ErrorCode.E_COLOR_INVALID, "rollback target not found",
                    {"material_id": mid, "version": target_version})
            historical = match[0]
            spec = self._import_spec(historical.params)
            with self._lock:
                self._material_params[mid] = canon(historical.params)
                self._specs[mid] = spec
            affected[mid] = f"rollback-to-v{target_version}"
        with self._lock:
            all_ids = sorted(self._material_params)
            old_versions = dict(old_release.versions) if old_release else {}
        for mid in all_ids:
            if mid in affected:
                continue
            old_v = old_versions.get(mid)
            if old_v is None:
                affected[mid] = "new-material"
                continue
            if not old_v.dep_factors:
                affected[mid] = "conservative:dep-factors-missing"
                continue
            changed = [k for k in sorted(new_dep)
                       if old_v.dep_factors.get(k) != new_dep[k]]
            if changed:
                affected[mid] = "env-factors-changed:" + ",".join(changed)

        # 3. Recompute affected versions (cancel checkpoint per version).
        new_versions = {}
        reused = []
        recomputed = []
        journal_done = {}
        try:
            for mid in all_ids:
                if self._cancel_event.is_set():
                    self._append_trace({
                        "event": "cancelled",
                        "completed_versions": sorted(journal_done),
                        "kept_release": old_release.release_id
                        if old_release else None,
                    })
                    return old_release
                if self.recompute_delay:
                    import time
                    time.sleep(self.recompute_delay)
                old_v = old_versions.get(mid)
                if mid not in affected:
                    new_versions[mid] = old_v
                    reused.append(mid)
                    continue
                spec = self._specs[mid]
                result = run_chain(spec, new_env, self.caches)
                prev_version = old_v.version if old_v else 0
                prev_hash = old_v.content_hash if old_v else GENESIS_HASH
                rollback_from = None
                for rb_mid, rb_v in rollbacks:
                    if rb_mid == mid:
                        rollback_from = rb_v
                version = MaterialVersion(
                    material_id=mid,
                    version=prev_version + 1,
                    params=canon(self._material_params[mid]),
                    dep_factors=new_dep,
                    encoded=result["encoded"],
                    intermediates=result["intermediates"],
                    prev_hash=prev_hash,
                    status=STATUS_PUBLISHED,
                    recompute_reason=affected[mid],
                    rollback_from=rollback_from,
                )
                new_versions[mid] = version
                recomputed.append(mid)
                journal_done[mid] = version.version
                self._write_journal(old_release, new_env, new_versions,
                                    old_versions, affected, all_ids)
        except SpectralStudioError:
            raise  # E_COLOR_INVALID etc: nothing is published

        # 4. Atomic whole-set publish.
        with self._lock:
            release_id = (old_release.release_id + 1) if old_release else 1
            prev_chain = old_release.chain_hash if old_release else GENESIS_HASH
            merged_ui = dict(self.ui_state)
            if ui_state:
                merged_ui.update(ui_state)
            self.ui_state = merged_ui
            release = Release(release_id, new_versions, new_env.to_config(),
                              merged_ui, prev_chain)
            self.env = new_env
            self._release = release
            for mid in recomputed:
                self._history.setdefault(mid, []).append(new_versions[mid])
        if self.snapshot_path:
            persistence.save_snapshot(self, self.snapshot_path)
        self._append_trace({
            "event": "publish",
            "release_id": release.release_id,
            "chain_hash": release.chain_hash,
            "affected_versions": {mid: new_versions[mid].version
                                  for mid in sorted(affected)},
            "reused_count": len(reused),
            "recomputed_count": len(recomputed),
            "reused": sorted(reused),
            "recomputed": sorted(recomputed),
            "reasons": {mid: affected[mid] for mid in sorted(affected)},
            "versions": {mid: {
                "version": new_versions[mid].version,
                "content_hash": new_versions[mid].content_hash,
                "encoded": new_versions[mid].encoded,
                "dep_factors": new_versions[mid].dep_factors,
            } for mid in sorted(new_versions)},
            "cache_stats": dict(self.caches.stats),
            "ops": [canon(op) for op in ops],
        })
        return release

    def _write_journal(self, old_release, new_env, new_versions, old_versions,
                       affected, all_ids):
        """Crash journal: tentative release with still-pending versions
        marked incomplete, so a forced kill mid-recompute is recoverable."""
        if not self.snapshot_path:
            return
        tentative = {}
        for mid in all_ids:
            if mid in new_versions:
                tentative[mid] = new_versions[mid]
            elif mid in affected:
                old_v = old_versions.get(mid)
                tentative[mid] = MaterialVersion(
                    material_id=mid,
                    version=(old_v.version + 1) if old_v else 1,
                    params=canon(self._material_params[mid]),
                    dep_factors=new_env.dependency_factors(),
                    encoded={},
                    intermediates={},
                    prev_hash=old_v.content_hash if old_v else GENESIS_HASH,
                    status=STATUS_INCOMPLETE,
                    recompute_reason=affected[mid],
                )
            else:
                tentative[mid] = old_versions[mid]
        release_id = (old_release.release_id + 1) if old_release else 1
        prev_chain = old_release.chain_hash if old_release else GENESIS_HASH
        journal = Release(release_id, tentative, new_env.to_config(),
                          self.ui_state, prev_chain)
        # The serialized history must include the tentative versions so the
        # journal snapshot validates as a consistent chain.
        history = {mid: list(chain) for mid, chain in self._history.items()}
        for mid in affected:
            history.setdefault(mid, []).append(tentative[mid])
        persistence.save_snapshot(self, self.snapshot_path, release=journal,
                                  history=history)

    # ------------------------------------------------------------------
    # recovery
    # ------------------------------------------------------------------
    @classmethod
    def recover(cls, snapshot_path):
        data = persistence.load_snapshot(snapshot_path)
        library = cls(env=PipelineEnvironment.from_config(data["env"]),
                      snapshot_path=snapshot_path)
        persistence.restore_into(library, data)
        incomplete = [mid for mid, v in library._release.versions.items()
                      if v.status != STATUS_PUBLISHED]
        if incomplete:
            # Recompute only the versions judged incomplete at recovery.
            library._recompute_incomplete(sorted(incomplete))
        return library

    def _recompute_incomplete(self, incomplete_ids):
        with self._lock:
            release = self._release
            new_versions = dict(release.versions)
        recomputed = []
        for mid in incomplete_ids:
            spec = self._specs[mid]
            result = run_chain(spec, self.env, self.caches)
            old_v = release.versions[mid]
            new_versions[mid] = MaterialVersion(
                material_id=mid,
                version=old_v.version,
                params=old_v.params,
                dep_factors=old_v.dep_factors,
                encoded=result["encoded"],
                intermediates=result["intermediates"],
                prev_hash=old_v.prev_hash,
                status=STATUS_PUBLISHED,
                recompute_reason="recovery:incomplete-version",
            )
            recomputed.append(mid)
        with self._lock:
            recovered = Release(release.release_id, new_versions,
                                release.env_config, release.ui_state,
                                release.prev_chain_hash)
            self._release = recovered
            for mid in recomputed:
                self._history[mid][-1] = new_versions[mid]
        if self.snapshot_path:
            persistence.save_snapshot(self, self.snapshot_path)
        self._append_trace({
            "event": "recovery-recompute",
            "release_id": recovered.release_id,
            "recomputed": recomputed,
            "versions": {mid: {
                "version": new_versions[mid].version,
                "content_hash": new_versions[mid].content_hash,
                "encoded": new_versions[mid].encoded,
            } for mid in sorted(recomputed)},
        })
