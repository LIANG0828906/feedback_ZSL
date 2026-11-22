"""volview: volume-data display components for medical image reading.

Public surface:

* :class:`VolumeModel` / :class:`VolumeRegistry` -- validated volume loading
* :class:`ReadingSession` -- window/plane/thickness interaction + snapshots
* :class:`PlaneScheduler` -- bounded concurrent plane computation
* :class:`SnapshotStore` -- atomic, serialised, ordered snapshot persistence
* :mod:`volview.generator` -- deterministic sample volumes with ground truth
* :mod:`volview.inject` / :mod:`volview.verify` -- anomaly & reproducibility
"""

from .errors import ErrorCode, VolViewError
from .measure import Annotation, accuracy_bound
from .model import Manifest, VolumeModel, VolumeRegistry
from .scheduler import PlaneScheduler, PlaneTask
from .session import ReadingSession
from .snapshot import SNAPSHOT_SCHEMA_VERSION, SnapshotStore

__all__ = [
    "Annotation",
    "ErrorCode",
    "Manifest",
    "PlaneScheduler",
    "PlaneTask",
    "ReadingSession",
    "SNAPSHOT_SCHEMA_VERSION",
    "SnapshotStore",
    "VolViewError",
    "VolumeModel",
    "VolumeRegistry",
    "accuracy_bound",
]
