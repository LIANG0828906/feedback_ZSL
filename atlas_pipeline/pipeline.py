from __future__ import annotations

import copy
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping, Sequence

import numpy as np

from .codec import decode_atlas_bytes
from .color import ColorChain, TileTransformResult, matrix_digest
from .constants import TONE_CURVES_WITH_COMPRESSION
from .errors import AtlasError, ErrorCode
from .hashing import canonical_json_bytes, hash_value, sha256_bytes
from .model import (
    AtlasImage,
    CachedTile,
    TileErrorSummary,
    TileParams,
    TransformConfig,
    Version,
)
from .serialization import compute_chain_hash


@dataclass(frozen=True)
class Preview:
    version: Version
    encoded: np.ndarray
    srgb: np.ndarray
    error_heatmap: np.ndarray


@dataclass(frozen=True)
class VersionDiff:
    from_version: int
    to_version: int
    config_changes: dict[str, dict[str, Any]]
    tile_changes: dict[int, dict[str, dict[str, Any]]]
    affected_tiles: tuple[int, ...]
    content_changed: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "from_version": self.from_version,
            "to_version": self.to_version,
            "config_changes": copy.deepcopy(self.config_changes),
            "tile_changes": {
                str(tile): dict(changes) for tile, changes in self.tile_changes.items()
            },
            "affected_tiles": list(self.affected_tiles),
            "content_changed": self.content_changed,
        }


@dataclass(frozen=True)
class RecomputeCheck:
    version: int
    exact_hash_match: bool
    maximum_summary_delta: float
    tolerated: float
    reused_tiles: tuple[int, ...]
    recomputed_tiles: tuple[int, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "exact_hash_match": self.exact_hash_match,
            "maximum_summary_delta": self.maximum_summary_delta,
            "tolerated": self.tolerated,
            "reused_tiles": list(self.reused_tiles),
            "recomputed_tiles": list(self.recomputed_tiles),
        }


class ConversionJob:
    """A cancellable, checkpointable tile conversion transaction."""

    def __init__(
        self,
        pipeline: "AtlasPipeline",
        *,
        base_version: Version,
        config: TransformConfig,
        tile_params: tuple[TileParams, ...],
        affected_tiles: tuple[int, ...],
        reason: str,
        version_number: int,
        rollback_of: int | None,
        local_cache: dict[str, CachedTile] | None = None,
        completed_tiles: Sequence[int] | None = None,
        owns_guard: bool = True,
    ) -> None:
        self.pipeline = pipeline
        self.base_version = base_version
        self.config = config
        self.tile_params = tile_params
        self.affected_tiles = affected_tiles
        self.reason = reason
        self.version_number = version_number
        self.rollback_of = rollback_of
        self.owns_guard = owns_guard
        self.chain = pipeline._make_chain(config)
        self.keys_by_tile = tuple(
            pipeline._tile_cache_key(index, self.chain, params)
            for index, params in enumerate(tile_params)
        )
        self.local_cache: dict[str, CachedTile] = dict(local_cache or {})
        self.completed_tiles: list[int] = list(completed_tiles or ())
        self._completed_set = set(self.completed_tiles)
        self._position = 0
        self._cancel_event = threading.Event()
        self._lock = threading.RLock()
        self.status = "running"
        self.error: AtlasError | None = None
        self.thread: threading.Thread | None = None

    @property
    def completed_count(self) -> int:
        with self._lock:
            return len(self._completed_set)

    @property
    def remaining_tiles(self) -> tuple[int, ...]:
        with self._lock:
            return tuple(
                tile for tile in self.affected_tiles if tile not in self._completed_set
            )

    def cancel(self) -> None:
        self._cancel_event.set()
        if self.status in ("running", "queued"):
            self.status = "cancelling"

    def _check_cancelled(self) -> None:
        if self._cancel_event.is_set():
            self.status = "cancelled"
            raise AtlasError(
                ErrorCode.E_CANCELLED,
                "conversion was cancelled before publication",
                details={"version": self.version_number},
            )

    def step_tile(self) -> bool:
        """Convert one still-missing affected tile. Returns False when done."""

        self._check_cancelled()
        with self._lock:
            while (
                self._position < len(self.affected_tiles)
                and self.affected_tiles[self._position] in self._completed_set
            ):
                self._position += 1
            if self._position >= len(self.affected_tiles):
                return False
            index = self.affected_tiles[self._position]
            key = self.keys_by_tile[index]
        result = self.pipeline._convert_tile(index, self.chain, self.tile_params[index])
        with self._lock:
            cached = CachedTile.build(
                key=key,
                tile=index,
                encoded=result.encoded,
                summary=result.summary,
            )
            self.local_cache[key] = cached
            self._completed_set.add(index)
            self.completed_tiles.append(index)
            self._position += 1
            more = self._position < len(self.affected_tiles)
        self.pipeline._checkpoint()
        return more

    def run_to_completion(self) -> Version:
        try:
            while self.step_tile():
                pass
            return self.finish()
        except AtlasError:
            if self.status != "cancelled":
                self.status = "failed"
            self.pipeline._release_job(self)
            raise

    def finish(self) -> Version:
        self._check_cancelled()
        with self._lock:
            missing = [
                tile
                for tile in self.affected_tiles
                if tile not in self._completed_set
            ]
        if missing:
            raise AtlasError(
                ErrorCode.E_USAGE,
                "cannot publish a conversion with unfinished affected tiles",
                field="affected_tiles",
                details={"missing": missing},
            )
        version = self.pipeline._publish_job(self)
        self.status = "published"
        return version

    def progress_dict(self) -> dict[str, Any]:
        with self._lock:
            return {
                "base_version": self.base_version.number,
                "config": self.config.to_dict(),
                "tile_params": [params.to_dict() for params in self.tile_params],
                "affected_tiles": list(self.affected_tiles),
                "completed_tiles": list(self.completed_tiles),
                "reason": self.reason,
                "version_number": self.version_number,
                "rollback_of": self.rollback_of,
                "local_cache_keys": [
                    self.keys_by_tile[tile] for tile in self.completed_tiles
                ],
            }

    def snapshot_state(self) -> tuple[dict[str, Any], dict[str, CachedTile]]:
        with self._lock:
            progress = self.progress_dict()
            completed = {
                self.keys_by_tile[tile]: self.local_cache[self.keys_by_tile[tile]]
                for tile in self.completed_tiles
            }
            return progress, completed

    def _run_async(self) -> None:
        try:
            self.run_to_completion()
        except AtlasError as exc:
            self.error = exc
            self.status = "cancelled" if exc.code == ErrorCode.E_CANCELLED else "failed"
        except Exception as exc:  # pragma: no cover - defensive for GUI thread
            self.error = AtlasError(
                ErrorCode.E_USAGE,
                f"unexpected conversion failure: {exc}",
            )
            self.status = "failed"
            self.pipeline._release_job(self)


class AtlasPipeline:
    """Immutable-version HDR atlas conversion pipeline."""

    def __init__(self, atlas: AtlasImage, *, initialize: bool = True) -> None:
        self._atlas = atlas
        self._publish_lock = threading.RLock()
        self._submission_lock = threading.Lock()
        self._active_job: ConversionJob | None = None
        self._versions: tuple[Version, ...] = ()
        self._cache: dict[str, CachedTile] = {}
        self._current_version = 0
        self._trace: list[dict[str, Any]] = []
        self._snapshot_lock = threading.Lock()
        self._checkpoint_path: Path | None = None
        if initialize:
            self._initialize_first_version()

    @property
    def atlas(self) -> AtlasImage:
        return self._atlas

    @property
    def current_version_number(self) -> int:
        with self._publish_lock:
            return self._current_version

    @property
    def current_version_record(self) -> Version:
        with self._publish_lock:
            return self._versions[self._current_version - 1]

    @property
    def versions(self) -> tuple[Version, ...]:
        with self._publish_lock:
            return self._versions

    @property
    def active_job(self) -> ConversionJob | None:
        with self._publish_lock:
            return self._active_job

    def _make_chain(self, config: TransformConfig) -> ColorChain:
        return ColorChain(
            source_space=self._atlas.header.color_space,
            source_transfer=self._atlas.header.transfer_function,
            source_peak_nits=self._atlas.header.source_peak_nits,
            config=config,
        )

    def _tile_cache_key(
        self,
        index: int,
        chain: ColorChain,
        params: TileParams,
    ) -> str:
        return hash_value(
            {
                "atlas": self._atlas.fingerprint,
                "tile": index,
                "chain": chain.digest,
                "bit_depth": self._atlas.header.bit_depth,
                "params": params.to_dict(),
            }
        )

    def _convert_tile(
        self,
        index: int,
        chain: ColorChain,
        params: TileParams,
    ) -> TileTransformResult:
        return chain.transform_tile(
            self._atlas.tile_linear(index),
            params,
            bit_depth=self._atlas.header.bit_depth,
            tile_index=index,
            reject_exposure_overflow=True,
        )

    def _default_config(self) -> TransformConfig:
        return TransformConfig(
            target_color_space=self._atlas.header.color_space,
            target_transfer=self._atlas.header.transfer_function,
            target_peak_nits=float(self._atlas.header.source_peak_nits),
            source_white_point="D65",
            target_white_point="D65",
            adaptation="bradford",
        )

    def _default_params(self) -> tuple[TileParams, ...]:
        return tuple(TileParams() for _ in self._atlas.tiles)

    def _initialize_first_version(self) -> None:
        config = self._default_config()
        params = self._default_params()
        chain = self._make_chain(config)
        local_cache: dict[str, CachedTile] = {}
        keys = tuple(
            self._tile_cache_key(index, chain, tile_params)
            for index, tile_params in enumerate(params)
        )
        for index in range(self._atlas.tile_count):
            result = self._convert_tile(index, chain, params[index])
            local_cache[keys[index]] = CachedTile.build(
                key=keys[index],
                tile=index,
                encoded=result.encoded,
                summary=result.summary,
            )
        version = self._build_version(
            number=1,
            parent=None,
            reason="initial",
            rollback_of=None,
            config=config,
            params=params,
            chain=chain,
            keys=keys,
            affected=tuple(range(self._atlas.tile_count)),
            reused=(),
            cache=local_cache,
        )
        self._versions = (version,)
        self._cache = local_cache
        self._current_version = 1
        self._trace.append(self._trace_entry(version))

    def _validate_params(
        self,
        config: TransformConfig,
        params: tuple[TileParams, ...],
    ) -> TransformConfig:
        config = config.validate(source_peak_nits=self._atlas.header.source_peak_nits)
        if len(params) != self._atlas.tile_count:
            missing = sorted(set(range(self._atlas.tile_count)) - set(range(len(params))))
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "tile parameter table does not match the atlas tile grid",
                field="tile_params",
                tile=missing[0] if missing else len(params),
                details={
                    "expected": self._atlas.tile_count,
                    "actual": len(params),
                },
            )
        for index, item in enumerate(params):
            item.validate(tile=index)
        target_peak = float(config.target_peak_nits)
        source_peak = float(self._atlas.header.source_peak_nits)
        if target_peak < source_peak:
            for index, item in enumerate(params):
                if item.tone_curve not in TONE_CURVES_WITH_COMPRESSION:
                    raise AtlasError(
                        ErrorCode.E_PARAM_CONFLICT,
                        "target peak is below source peak but the tile tone curve cannot compress",
                        field="tone_curve",
                        tile=index,
                        details={
                            "target_peak_nits": target_peak,
                            "source_peak_nits": source_peak,
                            "tone_curve": item.tone_curve,
                            "compression_curves": sorted(TONE_CURVES_WITH_COMPRESSION),
                        },
                    )
        return config

    def _merge_config(
        self,
        base: TransformConfig,
        value: TransformConfig | Mapping[str, Any] | None,
    ) -> TransformConfig:
        if value is None:
            return base
        if isinstance(value, TransformConfig):
            return value
        if not isinstance(value, Mapping):
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "config must be a TransformConfig or mapping",
                field="config",
            )
        merged = base.to_dict()
        allowed = set(merged)
        unknown = sorted(set(value) - allowed)
        if unknown:
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                f"unknown config field(s): {', '.join(unknown)}",
                field=unknown[0],
            )
        merged.update(dict(value))
        return TransformConfig.from_mapping(merged)

    def _parse_tile_value(
        self,
        index: int,
        base: TileParams,
        value: TileParams | Mapping[str, Any],
    ) -> TileParams:
        if isinstance(value, TileParams):
            return value.validate(tile=index)
        if not isinstance(value, Mapping):
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                "tile update must be a TileParams or mapping",
                field="tile_params",
                tile=index,
            )
        merged = base.to_dict()
        allowed = set(merged)
        unknown = sorted(set(value) - allowed)
        if unknown:
            raise AtlasError(
                ErrorCode.E_PARAM_CONFLICT,
                f"unknown tile parameter field {unknown[0]!r}",
                field=unknown[0],
                tile=index,
            )
        merged.update(dict(value))
        return TileParams.from_mapping(merged).validate(tile=index)

    def _resolve_submission(
        self,
        base: Version,
        config_value: TransformConfig | Mapping[str, Any] | None,
        tile_updates: Mapping[int, TileParams | Mapping[str, Any]] | None,
        full_tile_params: (
            Mapping[int, TileParams | Mapping[str, Any]]
            | Sequence[TileParams | Mapping[str, Any]]
            | None
        ),
    ) -> tuple[TransformConfig, tuple[TileParams, ...]]:
        config = self._merge_config(base.config, config_value)
        if full_tile_params is not None:
            if isinstance(full_tile_params, Mapping):
                try:
                    normalized = {
                        int(index): value for index, value in full_tile_params.items()
                    }
                except (TypeError, ValueError):
                    raise AtlasError(
                        ErrorCode.E_PARAM_CONFLICT,
                        "tile parameter table contains a non-integer tile index",
                        field="tile_params",
                    ) from None
                expected = set(range(self._atlas.tile_count))
                actual = set(normalized)
                if actual != expected:
                    missing = sorted(expected - actual)
                    extra = sorted(actual - expected)
                    raise AtlasError(
                        ErrorCode.E_PARAM_CONFLICT,
                        "tile parameter table keys do not match the atlas tile grid",
                        field="tile_params",
                        tile=(missing[0] if missing else extra[0]),
                        details={"missing": missing, "extra": extra},
                    )
                values: list[TileParams | Mapping[str, Any]] = [
                    normalized[index] for index in range(self._atlas.tile_count)
                ]
            else:
                values = list(full_tile_params)
                if len(values) != self._atlas.tile_count:
                    raise AtlasError(
                        ErrorCode.E_PARAM_CONFLICT,
                        "tile parameter table length does not match the atlas tile grid",
                        field="tile_params",
                        tile=min(len(values), self._atlas.tile_count),
                        details={
                            "expected": self._atlas.tile_count,
                            "actual": len(values),
                        },
                    )
            params = tuple(
                self._parse_tile_value(index, base.tile_params[index], value)
                for index, value in enumerate(values)
            )
        else:
            params = tuple(base.tile_params)
        if tile_updates is not None:
            mutable = list(params)
            for raw_index, value in tile_updates.items():
                try:
                    index = int(raw_index)
                except (TypeError, ValueError):
                    raise AtlasError(
                        ErrorCode.E_PARAM_CONFLICT,
                        "tile update index must be an integer",
                        field="tile_updates",
                    ) from None
                if index < 0 or index >= self._atlas.tile_count:
                    raise AtlasError(
                        ErrorCode.E_PARAM_CONFLICT,
                        "tile update index is outside the atlas tile grid",
                        field="tile_updates",
                        tile=index,
                        details={"tile_count": self._atlas.tile_count},
                    )
                mutable[index] = self._parse_tile_value(index, mutable[index], value)
            params = tuple(mutable)
        config = self._validate_params(config, params)
        return config, params

    def _affected_tiles(
        self,
        base: Version,
        config: TransformConfig,
        params: tuple[TileParams, ...],
    ) -> tuple[int, ...]:
        if config.shared_key != base.config.shared_key:
            return tuple(range(self._atlas.tile_count))
        return tuple(
            index
            for index, item in enumerate(params)
            if item != base.tile_params[index]
        )

    def begin_submission(
        self,
        *,
        config: TransformConfig | Mapping[str, Any] | None = None,
        tile_updates: Mapping[int, TileParams | Mapping[str, Any]] | None = None,
        tile_params: (
            Mapping[int, TileParams | Mapping[str, Any]]
            | Sequence[TileParams | Mapping[str, Any]]
            | None
        ) = None,
        expected_version: int | None = None,
        reason: str = "submit",
    ) -> ConversionJob:
        if not self._submission_lock.acquire(blocking=False):
            raise AtlasError(
                ErrorCode.E_BUSY,
                "another conversion task is active for this atlas",
            )
        try:
            with self._publish_lock:
                if expected_version is not None and int(expected_version) != self._current_version:
                    raise AtlasError(
                        ErrorCode.E_VERSION_STALE,
                        "submission is based on a version that is no longer current",
                        field="expected_version",
                        details={
                            "expected": int(expected_version),
                            "current": self._current_version,
                        },
                    )
                base = self._versions[self._current_version - 1]
                resolved_config, resolved_params = self._resolve_submission(
                    base,
                    config,
                    tile_updates,
                    tile_params,
                )
                affected = self._affected_tiles(base, resolved_config, resolved_params)
                chain = self._make_chain(resolved_config)
                keys = tuple(
                    self._tile_cache_key(index, chain, item)
                    for index, item in enumerate(resolved_params)
                )
                local_cache: dict[str, CachedTile] = {}
                missing_key_tiles: list[int] = []
                for index, key in enumerate(keys):
                    cached = self._cache.get(key)
                    if cached is None:
                        missing_key_tiles.append(index)
                    else:
                        local_cache[key] = cached
                if affected != tuple(missing_key_tiles):
                    # Shared-chain changes produce new keys for every tile.  Any
                    # missing non-affected key signals a cache/manifest mismatch.
                    unexpected = sorted(set(missing_key_tiles) - set(affected))
                    if unexpected:
                        raise AtlasError(
                            ErrorCode.E_VERSION_STALE,
                            "tile cache does not match the requested version state",
                            field="tile_hashes",
                            tile=unexpected[0],
                        )
                job = ConversionJob(
                    self,
                    base_version=base,
                    config=resolved_config,
                    tile_params=resolved_params,
                    affected_tiles=tuple(affected),
                    reason=str(reason),
                    version_number=len(self._versions) + 1,
                    rollback_of=None,
                    local_cache=local_cache,
                )
                self._active_job = job
            self._checkpoint()
            return job
        except BaseException:
            with self._publish_lock:
                self._active_job = None
            self._submission_lock.release()
            raise

    def submit(
        self,
        *,
        config: TransformConfig | Mapping[str, Any] | None = None,
        tile_updates: Mapping[int, TileParams | Mapping[str, Any]] | None = None,
        tile_params: (
            Mapping[int, TileParams | Mapping[str, Any]]
            | Sequence[TileParams | Mapping[str, Any]]
            | None
        ) = None,
        expected_version: int | None = None,
        reason: str = "submit",
    ) -> Version:
        job = self.begin_submission(
            config=config,
            tile_updates=tile_updates,
            tile_params=tile_params,
            expected_version=expected_version,
            reason=reason,
        )
        try:
            return job.run_to_completion()
        except BaseException:
            self._release_job(job)
            raise

    def submit_async(
        self,
        *,
        config: TransformConfig | Mapping[str, Any] | None = None,
        tile_updates: Mapping[int, TileParams | Mapping[str, Any]] | None = None,
        tile_params: (
            Mapping[int, TileParams | Mapping[str, Any]]
            | Sequence[TileParams | Mapping[str, Any]]
            | None
        ) = None,
        expected_version: int | None = None,
        reason: str = "submit",
    ) -> ConversionJob:
        job = self.begin_submission(
            config=config,
            tile_updates=tile_updates,
            tile_params=tile_params,
            expected_version=expected_version,
            reason=reason,
        )
        job.thread = threading.Thread(
            target=job._run_async,
            name=f"atlas-conversion-v{job.version_number}",
            daemon=True,
        )
        job.thread.start()
        return job

    def cancel_current(self) -> bool:
        with self._publish_lock:
            job = self._active_job
        if job is None:
            return False
        job.cancel()
        return True

    def _release_job(self, job: ConversionJob) -> None:
        with self._publish_lock:
            if self._active_job is job:
                self._active_job = None
        if job.owns_guard:
            try:
                self._submission_lock.release()
            except RuntimeError:
                pass
        self._checkpoint()

    def _build_version(
        self,
        *,
        number: int,
        parent: Version | None,
        reason: str,
        rollback_of: int | None,
        config: TransformConfig,
        params: tuple[TileParams, ...],
        chain: ColorChain,
        keys: tuple[str, ...],
        affected: tuple[int, ...],
        reused: tuple[int, ...],
        cache: Mapping[str, CachedTile],
    ) -> Version:
        tile_hashes: list[str] = []
        summaries: list[TileErrorSummary] = []
        for index, key in enumerate(keys):
            cached = cache.get(key)
            if cached is None:
                raise AtlasError(
                    ErrorCode.E_VERSION_STALE,
                    "published version references a missing tile cache entry",
                    field="tile_hashes",
                    tile=index,
                )
            if cached.tile != index:
                raise AtlasError(
                    ErrorCode.E_VERSION_STALE,
                    "tile cache entry belongs to another tile index",
                    field="tile_hashes",
                    tile=index,
                )
            tile_hashes.append(cached.tile_hash)
            summaries.append(cached.summary)
        content_hash = hash_value(
            {
                "tiles": [
                    {"tile": index, "hash": digest}
                    for index, digest in enumerate(tile_hashes)
                ]
            }
        )
        transformation_id = hash_value(
            {
                "atlas_fingerprint": self._atlas.fingerprint,
                "config": config.to_dict(),
                "tile_params": [item.to_dict() for item in params],
                "bit_depth": self._atlas.header.bit_depth,
            }
        )
        error_summary_hash = hash_value(
            [summary.to_dict() for summary in summaries]
        )
        parent_chain_hash = parent.chain_hash if parent is not None else None
        parent_version = parent.number if parent is not None else None
        chain_digests = {
            "source_payload": self._atlas.payload_hash,
            "source_linearization": hash_value(
                {
                    "transfer": self._atlas.header.transfer_function,
                    "peak_nits": self._atlas.header.source_peak_nits,
                    "payload": self._atlas.payload_hash,
                }
            ),
            "base_matrix": matrix_digest(chain.base_matrix),
            "white_adaptation": matrix_digest(chain.adaptation_matrix),
            "shared_matrix": matrix_digest(chain.matrix),
            "tone_map": hash_value(
                {
                    "config_peak": config.target_peak_nits,
                    "params": [item.to_dict() for item in params],
                }
            ),
            "tile_content": content_hash,
        }
        draft = Version(
            number=number,
            parent_version=parent_version,
            parent_chain_hash=parent_chain_hash,
            chain_hash="",
            atlas_fingerprint=self._atlas.fingerprint,
            reason=reason,
            rollback_of=rollback_of,
            config=config,
            tile_params=params,
            affected_tiles=affected,
            reused_tiles=reused,
            output_tile_hashes=tuple(tile_hashes),
            content_hash=content_hash,
            transformation_id=transformation_id,
            chain_digests=chain_digests,
            tile_summaries=tuple(summaries),
            error_summary_hash=error_summary_hash,
        )
        chain_hash = compute_chain_hash(draft)
        from dataclasses import replace

        return replace(draft, chain_hash=chain_hash)

    def _publish_job(self, job: ConversionJob) -> Version:
        with self._publish_lock:
            if job.version_number != len(self._versions) + 1:
                raise AtlasError(
                    ErrorCode.E_VERSION_STALE,
                    "conversion transaction targets a stale version number",
                    field="version",
                    details={
                        "transaction_version": job.version_number,
                        "expected_version": len(self._versions) + 1,
                    },
                )
            base = self._versions[job.base_version.number - 1]
            if base.number != self._current_version or base.number != job.base_version.number:
                raise AtlasError(
                    ErrorCode.E_VERSION_STALE,
                    "conversion transaction base version is no longer current",
                    field="parent_version",
                    details={
                        "base": job.base_version.number,
                        "current": self._current_version,
                    },
                )
            affected = job.affected_tiles
            reused = tuple(
                index for index in range(self._atlas.tile_count) if index not in affected
            )
            version = self._build_version(
                number=job.version_number,
                parent=base,
                reason=job.reason,
                rollback_of=job.rollback_of,
                config=job.config,
                params=job.tile_params,
                chain=job.chain,
                keys=job.keys_by_tile,
                affected=affected,
                reused=reused,
                cache=job.local_cache,
            )
            new_cache = dict(self._cache)
            new_cache.update(job.local_cache)
            new_versions = self._versions + (version,)
            # Publish all pieces together under the same lock. Preview readers
            # take this lock once and therefore see either the old or new state.
            self._cache = new_cache
            self._versions = new_versions
            self._current_version = version.number
            self._trace.append(self._trace_entry(version))
            if self._active_job is job:
                self._active_job = None
            if job.owns_guard:
                try:
                    self._submission_lock.release()
                except RuntimeError:
                    pass
        self._checkpoint()
        return version

    def _trace_entry(self, version: Version) -> dict[str, Any]:
        return {
            "event": "publish",
            "version": version.number,
            "parent_version": version.parent_version,
            "reason": version.reason,
            "rollback_of": version.rollback_of,
            "affected_tiles": list(version.affected_tiles),
            "reused_tiles": list(version.reused_tiles),
            "content_hash": version.content_hash,
            "chain_hash": version.chain_hash,
        }

    def get_version(self, number: int) -> Version:
        with self._publish_lock:
            try:
                return self._versions[int(number) - 1]
            except (IndexError, ValueError):
                raise AtlasError(
                    ErrorCode.E_VERSION_STALE,
                    f"version {number} does not exist",
                    field="version",
                ) from None

    def diff(self, from_version: int, to_version: int) -> VersionDiff:
        with self._publish_lock:
            source = self.get_version(from_version)
            target = self.get_version(to_version)
        config_changes: dict[str, dict[str, Any]] = {}
        source_config = source.config.to_dict()
        target_config = target.config.to_dict()
        for key in sorted(source_config):
            if source_config[key] != target_config[key]:
                config_changes[key] = {
                    "from": source_config[key],
                    "to": target_config[key],
                }
        tile_changes: dict[int, dict[str, dict[str, Any]]] = {}
        for index, (old, new) in enumerate(zip(source.tile_params, target.tile_params)):
            old_values = old.to_dict()
            new_values = new.to_dict()
            changes = {
                key: {"from": old_values[key], "to": new_values[key]}
                for key in sorted(old_values)
                if old_values[key] != new_values[key]
            }
            if changes:
                tile_changes[index] = changes
        return VersionDiff(
            from_version=source.number,
            to_version=target.number,
            config_changes=config_changes,
            tile_changes=tile_changes,
            affected_tiles=target.affected_tiles,
            content_changed=source.content_hash != target.content_hash,
        )

    def rollback(self, version_number: int) -> Version:
        if not self._submission_lock.acquire(blocking=False):
            raise AtlasError(
                ErrorCode.E_BUSY,
                "another conversion task is active for this atlas",
            )
        try:
            with self._publish_lock:
                target = self.get_version(version_number)
                base = self._versions[self._current_version - 1]
                chain = self._make_chain(target.config)
                keys = tuple(
                    self._tile_cache_key(index, chain, params)
                    for index, params in enumerate(target.tile_params)
                )
                local_cache: dict[str, CachedTile] = {}
                for index, key in enumerate(keys):
                    cached = self._cache.get(key)
                    if cached is None:
                        raise AtlasError(
                            ErrorCode.E_VERSION_STALE,
                            "rollback target tile output is no longer cached",
                            field="tile_hashes",
                            tile=index,
                        )
                    local_cache[key] = cached
                new_version = self._build_version(
                    number=len(self._versions) + 1,
                    parent=base,
                    reason=f"rollback:{target.number}",
                    rollback_of=target.number,
                    config=target.config,
                    params=target.tile_params,
                    chain=chain,
                    keys=keys,
                    affected=(),
                    reused=tuple(range(self._atlas.tile_count)),
                    cache=local_cache,
                )
                self._cache.update(local_cache)
                self._versions = self._versions + (new_version,)
                self._current_version = new_version.number
                self._trace.append(self._trace_entry(new_version))
            self._checkpoint()
            return new_version
        finally:
            try:
                self._submission_lock.release()
            except RuntimeError:
                pass

    def get_preview(self, version_number: int | None = None) -> Preview:
        with self._publish_lock:
            number = self._current_version if version_number is None else int(version_number)
            version = self.get_version(number)
            chain = self._make_chain(version.config)
            encoded = np.empty(self._atlas.header.shape, dtype=np.float16)
            summaries: list[int] = []
            for index, params in enumerate(version.tile_params):
                key = self._tile_cache_key(index, chain, params)
                cached = self._cache.get(key)
                if cached is None:
                    raise AtlasError(
                        ErrorCode.E_VERSION_STALE,
                        "preview tile output is missing from the cache",
                        field="tile_hashes",
                        tile=index,
                        details={"version": version.number},
                    )
                if cached.tile_hash != version.output_tile_hashes[index]:
                    raise AtlasError(
                        ErrorCode.E_VERSION_STALE,
                        "preview tile hash does not match the published manifest",
                        field="tile_hashes",
                        tile=index,
                        details={"version": version.number},
                    )
                spec = self._atlas.tiles[index]
                encoded[spec.y0 : spec.y1, spec.x0 : spec.x1, :] = cached.encoded
                summaries.append(index)
            if summaries != list(range(self._atlas.tile_count)):
                raise AtlasError(
                    ErrorCode.E_VERSION_STALE,
                    "preview did not cover every published tile",
                    field="tile_hashes",
                )
            srgb = chain.preview_srgb(encoded)
            heatmap = np.zeros(
                (self._atlas.header.height, self._atlas.header.width),
                dtype=np.float32,
            )
            max_code = float((1 << self._atlas.header.bit_depth) - 1)
            for spec, summary in zip(self._atlas.tiles, version.tile_summaries):
                heatmap[spec.y0 : spec.y1, spec.x0 : spec.x1] = min(
                    1.0,
                    summary.max_code_deviation / max_code,
                )
            return Preview(version=version, encoded=encoded, srgb=srgb, error_heatmap=heatmap)

    def capture_for_export(
        self,
        *,
        version_number: int | None = None,
        expected_version: int | None = None,
        expected_content_hash: str | None = None,
        expected_manifest: Mapping[int, str] | None = None,
    ) -> Preview:
        with self._publish_lock:
            current = self._current_version
            if expected_version is not None and int(expected_version) != current:
                raise AtlasError(
                    ErrorCode.E_VERSION_STALE,
                    "export expected a version that is no longer current",
                    field="version",
                    details={
                        "expected": int(expected_version),
                        "current": current,
                    },
                )
            number = current if version_number is None else int(version_number)
            version = self.get_version(number)
            if (
                expected_content_hash is not None
                and str(expected_content_hash) != version.content_hash
            ):
                raise AtlasError(
                    ErrorCode.E_VERSION_STALE,
                    "export expected a content hash that does not match the published version",
                    field="content_hash",
                    details={
                        "expected": str(expected_content_hash),
                        "actual": version.content_hash,
                    },
                )
            if expected_manifest is not None:
                expected = {
                    int(index): str(digest)
                    for index, digest in expected_manifest.items()
                }
                actual = {
                    index: digest
                    for index, digest in enumerate(version.output_tile_hashes)
                }
                if expected != actual:
                    mismatched = sorted(
                        index
                        for index in set(expected) | set(actual)
                        if expected.get(index) != actual.get(index)
                    )
                    raise AtlasError(
                        ErrorCode.E_VERSION_STALE,
                        "tile hash manifest does not match the published version",
                        field="tile_hashes",
                        tile=mismatched[0] if mismatched else None,
                        details={"mismatched_tiles": mismatched},
                    )
            return self.get_preview(version.number)

    def recompute_full_check(
        self,
        version_number: int | None = None,
        *,
        tolerated_summary_delta: float = 0.0,
    ) -> RecomputeCheck:
        with self._publish_lock:
            version = self.get_version(
                self._current_version if version_number is None else version_number
            )
            chain = self._make_chain(version.config)
        hashes: list[str] = []
        summaries: list[TileErrorSummary] = []
        for index, params in enumerate(version.tile_params):
            result = chain.transform_tile(
                self._atlas.tile_linear(index),
                params,
                bit_depth=self._atlas.header.bit_depth,
                tile_index=index,
                reject_exposure_overflow=True,
            )
            cached = CachedTile.build(
                key="recompute",
                tile=index,
                encoded=result.encoded,
                summary=result.summary,
            )
            hashes.append(cached.tile_hash)
            summaries.append(result.summary)
        exact = tuple(hashes) == version.output_tile_hashes
        maximum_delta = 0.0
        for fresh, persisted in zip(summaries, version.tile_summaries):
            maximum_delta = max(
                maximum_delta,
                abs(fresh.max_code_deviation - persisted.max_code_deviation),
                abs(fresh.mean_code_deviation - persisted.mean_code_deviation),
                abs(fresh.rmse_code_deviation - persisted.rmse_code_deviation),
            )
        reused = tuple(
            index
            for index in range(self._atlas.tile_count)
            if index in version.reused_tiles
        )
        recomputed = tuple(
            index
            for index in range(self._atlas.tile_count)
            if index in version.affected_tiles
        )
        return RecomputeCheck(
            version=version.number,
            exact_hash_match=exact,
            maximum_summary_delta=maximum_delta,
            tolerated=tolerated_summary_delta,
            reused_tiles=reused,
            recomputed_tiles=recomputed,
        )

    def trace_document(self) -> dict[str, Any]:
        with self._publish_lock:
            return {
                "format": "atlas-pipeline-trace",
                "format_version": 1,
                "atlas_fingerprint": self._atlas.fingerprint,
                "versions": copy.deepcopy(self._trace),
            }

    def write_trace(self, path: str | Path) -> str:
        from .codec import atomic_write_bytes

        payload = (
            __import__("json").dumps(
                self.trace_document(),
                indent=2,
                sort_keys=True,
                ensure_ascii=True,
            )
            + "\n"
        ).encode("ascii")
        atomic_write_bytes(path, payload)
        return sha256_bytes(payload)

    def save_snapshot(self, path: str | Path) -> str:
        from .storage import save_snapshot

        return save_snapshot(self, path)

    def set_checkpoint_path(self, path: str | Path | None) -> None:
        self._checkpoint_path = None if path is None else Path(path)

    def _checkpoint(self) -> None:
        if self._checkpoint_path is not None:
            self.save_snapshot(self._checkpoint_path)

    @classmethod
    def from_snapshot(
        cls,
        path: str | Path,
        *,
        resume: bool = True,
    ) -> "AtlasPipeline":
        from .storage import load_snapshot

        return load_snapshot(path, resume=resume)

    def replace_atlas(self, atlas_data: bytes) -> None:
        """Build a fresh pipeline from persisted atlas bytes.

        This helper is used by recovery code; it deliberately does not mutate an
        existing pipeline.
        """

        atlas = decode_atlas_bytes(atlas_data)
        self._atlas = atlas
        self._versions = ()
        self._cache = {}
        self._current_version = 0
        self._trace = []
        self._initialize_first_version()
