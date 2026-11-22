"""Configurable analysis parameters, validation and versioned snapshots.

Every user-tunable knob lives in :class:`AnalysisParams`.  Changing any knob
produces a new, immutable :class:`VersionSnapshot` with a fresh monotonically
increasing version number.  Preview jobs read the *latest* snapshot while a
precise run *locks* the snapshot it started with, so the two paths can never
mix parameter versions.
"""

from dataclasses import dataclass, field, replace
import hashlib

from .errors import ForensicsError, E_PARA

# Bumped only when the numeric analysis algorithms change their definition.
ALGORITHM_VERSION = "alg-1.0.0"

SUPPORTED_HASHES = ("sha256", "sha1", "md5")

# Declared, fixed tolerances used by the preview/precise comparison.
# Region membership must match exactly. Coarse statistics are compared against
# the shared kernel evaluated on the identical coarse block, so tolerances only
# cover floating-point rounding (the two are numerically the same computation).
DIVERGENCE_TOLERANCE = {
    "noise_var": 1e-6,
    "edge_consistency": 1e-9,
    "local_contrast": 1e-9,
    "score": 1e-12,
}


@dataclass(frozen=True)
class AnalysisParams:
    """All user-configurable parameters with their legal domains.

    Validation is performed eagerly in :meth:`validate` so an out-of-domain
    value always raises E_PARA naming the offending field, before any pixel
    work is done.
    """

    block_size: int = 32
    hash_algorithm: str = "sha256"
    fingerprint_granularity: int = 8
    noise_window: int = 3
    edge_window: int = 3
    contrast_window: int = 3
    noise_threshold: float = 100.0
    edge_threshold: float = 0.30
    contrast_threshold: float = 20.0
    # Integer downscale factor for the interactive preview path (1 = full res).
    preview_scale: int = 2

    def validate(self):
        if not isinstance(self.block_size, int) or self.block_size <= 0:
            raise ForensicsError(
                E_PARA,
                "block_size must be a positive integer, got %r" % self.block_size,
                field="block_size",
            )
        if self.hash_algorithm not in SUPPORTED_HASHES:
            raise ForensicsError(
                E_PARA,
                "hash_algorithm must be one of %s, got %r"
                % (SUPPORTED_HASHES, self.hash_algorithm),
                field="hash_algorithm",
            )
        if (
            not isinstance(self.fingerprint_granularity, int)
            or self.fingerprint_granularity <= 0
            or self.fingerprint_granularity > self.block_size
        ):
            raise ForensicsError(
                E_PARA,
                "fingerprint_granularity must be in [1, block_size=%d], got %r"
                % (self.block_size, self.fingerprint_granularity),
                field="fingerprint_granularity",
            )
        for name in ("noise_window", "edge_window", "contrast_window"):
            value = getattr(self, name)
            if not isinstance(value, int) or value <= 0 or value % 2 == 0:
                raise ForensicsError(
                    E_PARA,
                    "%s must be a positive odd integer, got %r" % (name, value),
                    field=name,
                )
        for name in ("noise_threshold", "edge_threshold", "contrast_threshold"):
            value = getattr(self, name)
            if not isinstance(value, (int, float)) or value < 0:
                raise ForensicsError(
                    E_PARA,
                    "%s must be a non-negative number, got %r" % (name, value),
                    field=name,
                )
        if not isinstance(self.preview_scale, int) or self.preview_scale <= 0:
            raise ForensicsError(
                E_PARA,
                "preview_scale must be a positive integer, got %r" % self.preview_scale,
                field="preview_scale",
            )
        return self

    def with_changes(self, **changes):
        """Return a new validated config with the given fields replaced."""
        return replace(self, **changes).validate()

    def fingerprint_dict(self):
        """Stable ordered mapping used for hashing / checkpoint identity."""
        return {
            "block_size": self.block_size,
            "hash_algorithm": self.hash_algorithm,
            "fingerprint_granularity": self.fingerprint_granularity,
            "noise_window": self.noise_window,
            "edge_window": self.edge_window,
            "contrast_window": self.contrast_window,
            "noise_threshold": self.noise_threshold,
            "edge_threshold": self.edge_threshold,
            "contrast_threshold": self.contrast_threshold,
            "preview_scale": self.preview_scale,
        }

    def content_signature(self):
        """Deterministic short hash of the *parameter* contents (not pixels)."""
        items = sorted(self.fingerprint_dict().items())
        payload = repr(items).encode("utf-8")
        return hashlib.sha256(payload).hexdigest()[:16]


@dataclass(frozen=True)
class VersionSnapshot:
    """An immutable, numbered snapshot of a parameter set."""

    version: int
    params: AnalysisParams
    signature: str
    algorithm_version: str = field(default=ALGORITHM_VERSION)


class VersionControl:
    """Issues monotonically increasing version snapshots.

    Editing any parameter (threshold / window / block size) calls
    :meth:`edit`, which validates the new config and returns a fresh snapshot.
    Re-applying an unchanged config returns the same version (no-op edits do
    not churn the cache).
    """

    def __init__(self, initial=None):
        self._counter = 0
        self._history = {}
        self._signature_to_version = {}
        initial = (initial or AnalysisParams()).validate()
        self._latest = self._issue(initial)

    def _issue(self, params):
        sig = params.content_signature()
        if sig in self._signature_to_version:
            return self._history[self._signature_to_version[sig]]
        self._counter += 1
        snap = VersionSnapshot(
            version=self._counter,
            params=params,
            signature=sig,
            algorithm_version=ALGORITHM_VERSION,
        )
        self._history[self._counter] = snap
        self._signature_to_version[sig] = self._counter
        return snap

    @property
    def latest(self):
        return self._latest

    def edit(self, **changes):
        """Create (or reuse) a version snapshot after applying ``changes``."""
        new_params = self._latest.params.with_changes(**changes)
        self._latest = self._issue(new_params)
        return self._latest

    def replace_params(self, params):
        params.validate()
        self._latest = self._issue(params)
        return self._latest

    def get(self, version):
        if version not in self._history:
            raise ForensicsError(
                E_VER,
                "unknown parameter version %r" % (version,),
                field="version",
            )
        return self._history[version]
