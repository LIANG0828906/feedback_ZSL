"""Block-based offline renderer with crash-resume snapshots."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .acoustics import BinauralPanner, FftConvolver, build_impulse_response
from .errors import RESUME_MISMATCH, BusinessError
from .model import build_frame_table, resample_frames_to_samples
from .wavio import read_wav, write_wav

SNAPSHOT_NAME = "snapshot.npz"
BLOCK_PREFIX = "block_"


def scene_digest(scene: dict) -> str:
    canonical = json.dumps(scene, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def soft_limit(signal: np.ndarray, ceiling: float = 0.97) -> np.ndarray:
    return ceiling * np.tanh(signal / ceiling)


@dataclass
class RenderResult:
    master_path: Path
    digest: str
    block_count: int
    total_samples: int
    resume_count: int
    graceful_interrupt: bool
    resume_mismatch: bool
    frame_table: dict


class SoundscapeRenderer:
    def __init__(self, scene: dict, base_dir: str | Path, work_dir: str | Path) -> None:
        self.scene = scene
        self.base_dir = Path(base_dir)
        self.work_dir = Path(work_dir)
        self.project = scene["project"]
        self.sample_rate = int(self.project["sample_rate"])
        self.block_samples = int(self.project["block_samples"])
        self.frame_rate = float(self.project["frame_rate"])
        self.duration = float(self.project["duration"])
        self.tail_samples = int(round(float(self.project.get("tail_seconds", 1.0)) * self.sample_rate))
        self.total_samples = int(round(self.duration * self.sample_rate)) + self.tail_samples
        self.block_count = int(np.ceil(self.total_samples / self.block_samples))
        self.digest = scene_digest(scene)
        self.interrupt_requested = False
        self.kill_after_block: int | None = None
        self.resume_count = 0
        self.graceful_interrupt = False
        self.resume_mismatch = False
        self.table = build_frame_table(scene)
        self.work_dir.mkdir(parents=True, exist_ok=True)

    def _block_path(self, index: int) -> Path:
        return self.work_dir / f"{BLOCK_PREFIX}{index:06d}.raw"

    def _snapshot_path(self) -> Path:
        return self.work_dir / SNAPSHOT_NAME

    def _build_convolvers(self) -> dict[str, dict[str, FftConvolver]]:
        seed = int(self.project["seed"])
        bank: dict[str, dict[str, FftConvolver]] = {}
        for source in self.scene["sources"]:
            per_source = {}
            for layer in self.scene["layers"]:
                impulse = build_impulse_response(
                    float(layer["reverb"]["rt60"]),
                    self.sample_rate,
                    seed,
                    layer["id"],
                )
                per_source[layer["id"]] = FftConvolver(impulse, self.block_samples)
            bank[source["id"]] = per_source
        return bank

    def _zone_weights_at_sample(self, sample_index: int) -> dict[str, float]:
        frame = min(int(sample_index * self.frame_rate / self.sample_rate), self.table["frame_count"] - 1)
        state = self.table["transition"][frame]
        if state.startswith("blend:"):
            left_id, right_id = state.split(":", 1)[1].split("->")
            x = self.table["listener_positions"][frame][0]
            planes = [layer["plane"] for layer in self.scene["layers"]]
            plane = next(layer["plane"] for layer in self.scene["layers"] if layer["id"] == right_id)
            width = float(self.project.get("crossfade_width", 0.5))
            alpha = float(np.clip((x - (plane - width)) / (2.0 * width), 0.0, 1.0))
            return {left_id: 1.0 - alpha, right_id: alpha}
        zone = self.table["zones"][frame]
        return {zone: 1.0}

    def _load_or_init(self) -> tuple[int, dict, dict]:
        snapshot = self._snapshot_path()
        if not snapshot.is_file():
            return 0, self._build_convolvers(), {
                source["id"]: BinauralPanner(self.sample_rate) for source in self.scene["sources"]
            }
        with np.load(snapshot, allow_pickle=False) as data:
            stored_digest = str(data["scene_digest"])
            completed = int(data["completed_blocks"])
            stored_block_samples = int(data["block_samples"])
            stored_rate = int(data["sample_rate"])
            stored_total = int(data["total_samples"])
            if (
                stored_digest != self.digest
                or stored_block_samples != self.block_samples
                or stored_rate != self.sample_rate
                or stored_total != self.total_samples
            ):
                self._discard_progress()
                self.resume_mismatch = True
                self.resume_count += 1
                return 0, self._build_convolvers(), {
                    source["id"]: BinauralPanner(self.sample_rate) for source in self.scene["sources"]
                }
            convolvers = self._build_convolvers()
            panners = {source["id"]: BinauralPanner(self.sample_rate) for source in self.scene["sources"]}
            convolver_state = json.loads(str(data["convolver_state"]))
            panner_state = json.loads(str(data["panner_state"]))
            for source_id, per_layer in convolver_state.items():
                for layer_id, state in per_layer.items():
                    convolvers[source_id][layer_id].load_state({
                        "history": np.asarray(state["history"], dtype=np.float64),
                        "impulse": np.asarray(state["impulse"], dtype=np.float64),
                    })
            for source_id, state in panner_state.items():
                panners[source_id].load_state({
                    "left_history": np.asarray(state["left_history"], dtype=np.float64),
                    "right_history": np.asarray(state["right_history"], dtype=np.float64),
                })
            self.resume_count = int(data["resume_count"]) + 1
            return completed, convolvers, panners

    def _discard_progress(self) -> None:
        for path in self.work_dir.glob(f"{BLOCK_PREFIX}*.raw"):
            path.unlink()
        snapshot = self._snapshot_path()
        if snapshot.is_file():
            snapshot.unlink()

    def _save_snapshot(self, completed_blocks: int, convolvers: dict, panners: dict) -> None:
        convolver_state = {
            source_id: {
                layer_id: {
                    "history": conv.state_dict()["history"].tolist(),
                    "impulse": conv.state_dict()["impulse"].tolist(),
                }
                for layer_id, conv in per_source.items()
            }
            for source_id, per_source in convolvers.items()
        }
        panner_state = {
            source_id: {
                "left_history": panner.state_dict()["left_history"].tolist(),
                "right_history": panner.state_dict()["right_history"].tolist(),
            }
            for source_id, panner in panners.items()
        }
        payload = {
            "scene_digest": np.array(self.digest),
            "completed_blocks": np.array(completed_blocks, dtype=np.int64),
            "block_samples": np.array(self.block_samples, dtype=np.int64),
            "sample_rate": np.array(self.sample_rate, dtype=np.int64),
            "total_samples": np.array(self.total_samples, dtype=np.int64),
            "resume_count": np.array(self.resume_count, dtype=np.int64),
            "convolver_state": np.array(json.dumps(convolver_state, sort_keys=True)),
            "panner_state": np.array(json.dumps(panner_state, sort_keys=True)),
        }
        tmp = self._snapshot_path().with_suffix(".tmp.npz")
        np.savez(tmp, **payload)
        tmp.replace(self._snapshot_path())

    def _render_source_block(self, source: dict, start: int, block_size: int,
                             convolvers: dict, panner: BinauralPanner) -> np.ndarray:
        source_id = source["id"]
        dry, _ = read_wav(self.base_dir / source["file"])
        padded = np.zeros(self.total_samples, dtype=np.float64)
        padded[: min(dry.size, self.total_samples)] = dry[: self.total_samples]
        block = padded[start : start + block_size]
        arrays = self.table["sources"][source_id]
        direct_gain = resample_frames_to_samples(
            arrays["direct_gain"], block_size, self.frame_rate, self.sample_rate
        )
        itd = resample_frames_to_samples(arrays["itd_s"], block_size, self.frame_rate, self.sample_rate)
        gain_left = resample_frames_to_samples(
            arrays["gain_left"], block_size, self.frame_rate, self.sample_rate
        )
        gain_right = resample_frames_to_samples(
            arrays["gain_right"], block_size, self.frame_rate, self.sample_rate
        )
        direct = panner.render(block * direct_gain, itd, gain_left, gain_right)

        wet_mono = np.zeros(block_size, dtype=np.float64)
        start_weights = self._zone_weights_at_sample(start)
        end_weights = self._zone_weights_at_sample(start + block_size - 1)
        layer_ids = sorted(set(start_weights) | set(end_weights))
        ramp = np.linspace(0.0, 1.0, block_size, dtype=np.float64)
        wet_curve = np.sqrt(resample_frames_to_samples(
            self.table["wet"], self.total_samples, self.frame_rate, self.sample_rate
        )[start : start + block_size])
        dry_curve = np.sqrt(np.maximum(1.0 - wet_curve ** 2, 0.0))
        for layer_id in layer_ids:
            convolved = convolvers[source_id][layer_id].process(block)
            start_weight = start_weights.get(layer_id, 0.0)
            end_weight = end_weights.get(layer_id, 0.0)
            weight_curve = start_weight + (end_weight - start_weight) * ramp
            wet_mono += convolved * weight_curve
        wet_mono *= wet_curve
        wet = panner.render(
            wet_mono,
            itd,
            np.ones(block_size, dtype=np.float64),
            np.ones(block_size, dtype=np.float64),
        )
        direct *= dry_curve[:, None]
        return direct + wet

    def render(self) -> RenderResult:
        completed, convolvers, panners = self._load_or_init()
        for block_index in range(completed, self.block_count):
            start = block_index * self.block_samples
            block_size = min(self.block_samples, self.total_samples - start)
            master_block = np.zeros((block_size, 2), dtype=np.float64)
            for source in self.scene["sources"]:
                master_block += self._render_source_block(
                    source, start, block_size, convolvers, panners[source["id"]]
                )
            master_block = soft_limit(master_block)
            self._block_path(block_index).write_bytes(
                np.ascontiguousarray(master_block, dtype=np.float32).tobytes()
            )
            completed = block_index + 1
            self._save_snapshot(completed, convolvers, panners)
            if self.kill_after_block is not None and block_index + 1 == self.kill_after_block:
                raise SystemExit(99)
            if self.interrupt_requested:
                self.graceful_interrupt = True
                break
        if self.graceful_interrupt:
            return RenderResult(
                master_path=self.work_dir / "master.wav",
                digest=self.digest,
                block_count=self.block_count,
                total_samples=self.total_samples,
                resume_count=self.resume_count,
                graceful_interrupt=True,
                resume_mismatch=self.resume_mismatch,
                frame_table=self.table,
            )
        return self._assemble()

    def _assemble(self) -> RenderResult:
        pieces = []
        for index in range(self.block_count):
            path = self._block_path(index)
            if not path.is_file():
                raise BusinessError(
                    RESUME_MISMATCH,
                    f"$.blocks[{index}]",
                    f"missing block {index} during assembly",
                )
            raw = np.frombuffer(path.read_bytes(), dtype=np.float32)
            pieces.append(raw.reshape(-1, 2).astype(np.float64))
        master = np.concatenate(pieces, axis=0)[: self.total_samples]
        master_path = self.work_dir / "master.wav"
        write_wav(master_path, master, self.sample_rate)
        return RenderResult(
            master_path=master_path,
            digest=self.digest,
            block_count=self.block_count,
            total_samples=self.total_samples,
            resume_count=self.resume_count,
            graceful_interrupt=self.graceful_interrupt,
            resume_mismatch=self.resume_mismatch,
            frame_table=self.table,
        )

