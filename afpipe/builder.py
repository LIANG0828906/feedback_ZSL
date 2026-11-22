"""Index build orchestration: bounded parallelism, serial commits, resume."""

from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field

from .checkpoint import CheckpointStore
from .config import Config
from .errors import AFPipeError
from .fingerprint import FingerprintTrack, FrameFingerprint
from .indexstore import IndexBuilder
from .manifest import Manifest, MediaFile
from .pipeline import ProcessedFile, process_file


@dataclass
class FileOutcome:
    file_id: str
    status: str                 # ok | failed | reused
    error_code: str | None = None
    message: str = ""
    frame_count: int = 0


@dataclass
class BuildReport:
    parameter_version: str
    manifest_fingerprint: str
    outcomes: list[FileOutcome] = field(default_factory=list)
    resumed: bool = False
    invalidation: dict | None = None
    failed_files: list[str] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "parameter_version": self.parameter_version,
            "manifest_fingerprint": self.manifest_fingerprint,
            "resumed": self.resumed,
            "invalidation": self.invalidation,
            "failed_files": self.failed_files,
            "outcomes": [vars(o) for o in self.outcomes],
        }


def _chunk(frames: list[FrameFingerprint], size: int):
    for i in range(0, len(frames), size):
        yield i // size, frames[i:i + size]


class IndexBuildManager:
    """Builds both checkpoint shards and the final index store."""

    def __init__(self, work_dir: str, index_dir: str, cfg: Config,
                 manifest: Manifest):
        cfg.validate()
        self.cfg = cfg
        self.manifest = manifest
        self.checkpoint = CheckpointStore(
            work_dir, cfg, manifest.fingerprint
        )
        self.index_dir = index_dir

    def build(self, *, role: str = "index", retries: int = 1) -> BuildReport:
        report = BuildReport(
            parameter_version=self.cfg.parameter_version(),
            manifest_fingerprint=self.manifest.fingerprint,
        )
        reusable = self.checkpoint.load()
        report.resumed = bool(reusable)
        report.invalidation = self.checkpoint.invalidation_record()

        files = [f for f in self.manifest.files if f.role == role]
        index_builder = IndexBuilder(
            self.index_dir, self.cfg, self.manifest.fingerprint
        )

        # Parallel extraction (bounded), but results are consumed strictly in
        # manifest order so checkpoint/index commits stay deterministic.
        pool = ThreadPoolExecutor(max_workers=self.cfg.max_workers)
        futures = {
            f.file_id: pool.submit(self._safe_process, f, retries)
            for f in files
        }
        try:
            for media in files:
                file_id = media.file_id
                if file_id in reusable:
                    frames = self.checkpoint.load_frames(reusable[file_id])
                    track_frames = frames
                    content_hash = reusable[file_id].content_hash
                    report.outcomes.append(FileOutcome(
                        file_id, "reused", frame_count=len(frames),
                    ))
                else:
                    result = futures[file_id].result()
                    if isinstance(result, AFPipeError):
                        self.checkpoint.mark_failed(
                            file_id, result.code, result.message
                        )
                        report.outcomes.append(FileOutcome(
                            file_id, "failed", result.code, result.message
                        ))
                        report.failed_files.append(file_id)
                        continue
                    processed: ProcessedFile = result
                    track_frames = processed.track.frames
                    content_hash = processed.content_hash
                    self._commit_shards(media, processed)
                    report.outcomes.append(FileOutcome(
                        file_id, "ok", frame_count=len(track_frames),
                    ))

                # Serial index entry write, stable manifest order.
                track = FingerprintTrack(
                    file_id, track_frames, 0,
                    self.cfg.duration_of_frames(len(track_frames)),
                )
                index_builder.add_track(media, track, content_hash)
        finally:
            pool.shutdown(wait=True)

        index_builder.commit()
        return report

    def _safe_process(self, media: MediaFile, retries: int):
        """Run extraction, retrying once; never raise into the pool result."""
        attempt = 0
        while True:
            try:
                return process_file(media, self.manifest, self.cfg)
            except AFPipeError as exc:
                attempt += 1
                if attempt > retries:
                    return exc

    def _commit_shards(self, media: MediaFile, processed: ProcessedFile) -> None:
        cp = self.checkpoint
        cp.begin_file(media.file_id, processed.content_hash)
        for shard_index, frames in _chunk(
            processed.track.frames, self.cfg.shard_frames
        ):
            cp.append_shard(
                media.file_id, processed.content_hash,
                shard_index, frames,
            )
        cp.complete_file(media.file_id, len(processed.track.frames))
