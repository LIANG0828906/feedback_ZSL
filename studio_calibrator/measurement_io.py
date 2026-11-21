from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List, Optional

import numpy as np
import soundfile as sf

from .errors import E_IO, E_LAYOUT_INVALID, E_PAIR_MISSING, BusinessError
from .util import file_hash, load_json, require_keys


PCM_16_SCALE = 32768.0
WAVE_51_ORDER = ("FL", "FR", "FC", "LFE", "BL", "BR")


@dataclass(frozen=True)
class PairMeasurement:
    source: str
    receiver: str
    path: Path
    path_label: str
    channel: int
    layout: str
    file_channels: int


@dataclass(frozen=True)
class MeasurementManifest:
    root: Path
    sample_rate: int
    speakers: tuple[str, ...]
    receivers: tuple[str, ...]
    main_receivers: Dict[str, str]
    stimuli: Dict[str, Path]
    pairs: tuple[PairMeasurement, ...]
    layout: str
    layout_description: str
    digest: str

    def pair(self, source: str, receiver: str) -> PairMeasurement:
        for pair in self.pairs:
            if pair.source == source and pair.receiver == receiver:
                return pair
        raise BusinessError(
            E_PAIR_MISSING,
            "measurement pair is absent from the selected layout",
            source=source,
            receiver=receiver,
        )


def _pcm16_channel(
    path: Path,
    channel: int,
    expected_sr: int,
    expected_layout: str,
    source: str,
    receiver: str,
) -> np.ndarray:
    try:
        info = sf.info(str(path))
    except Exception as exc:
        raise BusinessError(
            E_PAIR_MISSING,
            f"cannot open PCM WAV component: {exc}",
            source=source,
            receiver=receiver,
            details={"path": path.name},
        ) from exc
    if info.samplerate != expected_sr:
        raise BusinessError(
            E_PAIR_MISSING,
            "component sample rate does not match manifest",
            source=source,
            receiver=receiver,
            details={"expected": expected_sr, "actual": info.samplerate},
        )
    if info.subtype != "PCM_16":
        raise BusinessError(
            E_PAIR_MISSING,
            "component is not 16-bit PCM",
            source=source,
            receiver=receiver,
            details={"subtype": info.subtype},
        )
    if expected_layout == "mono" and info.channels != 1:
        raise BusinessError(
            E_LAYOUT_INVALID,
            "mono layout requires a one-channel WAV",
            source=source,
            receiver=receiver,
            details={"channels": info.channels, "path": path.name},
        )
    if expected_layout == "5.1" and info.channels != 6:
        raise BusinessError(
            E_LAYOUT_INVALID,
            "5.1 layout requires a six-channel WAV",
            source=source,
            receiver=receiver,
            details={"channels": info.channels, "path": path.name},
        )
    if not 0 <= channel < info.channels:
        raise BusinessError(
            E_LAYOUT_INVALID,
            "selected component channel is outside the WAV channel count",
            source=source,
            receiver=receiver,
            details={"channel": channel, "channels": info.channels},
        )
    try:
        data, sample_rate = sf.read(
            str(path), dtype="int16", always_2d=True, frames=-1
        )
    except Exception as exc:
        raise BusinessError(
            E_IO,
            f"failed to read PCM WAV component: {exc}",
            source=source,
            receiver=receiver,
            details={"path": path.name},
        ) from exc
    if sample_rate != expected_sr:
        raise BusinessError(
            E_PAIR_MISSING,
            "component sample rate changed between probe and read",
            source=source,
            receiver=receiver,
        )
    if data.size == 0:
        raise BusinessError(
            E_PAIR_MISSING,
            "component WAV has no samples",
            source=source,
            receiver=receiver,
            details={"path": path.name},
        )
    return data[:, channel].astype(np.float64) / PCM_16_SCALE


def read_component(
    manifest: MeasurementManifest,
    pair: PairMeasurement,
) -> np.ndarray:
    return _pcm16_channel(
        pair.path,
        pair.channel,
        manifest.sample_rate,
        pair.layout,
        pair.source,
        pair.receiver,
    )


def read_stimulus(manifest: MeasurementManifest, source: str) -> np.ndarray:
    path = manifest.stimuli.get(source)
    if path is None:
        raise BusinessError(
            E_PAIR_MISSING,
            "source stimulus is absent from manifest",
            source=source,
            details={"stimuli": sorted(manifest.stimuli)},
        )
    return _pcm16_channel(
        path,
        0,
        manifest.sample_rate,
        "mono",
        source,
        source,
    )


def _resolve(root: Path, relative: str) -> Path:
    path = (root / relative).resolve()
    try:
        path.relative_to(root.resolve())
    except ValueError as exc:
        raise BusinessError(
            E_LAYOUT_INVALID,
            "manifest path escapes the sample root",
            details={"path": relative},
        ) from exc
    return path


def load_manifest(path: Path, layout: str) -> MeasurementManifest:
    payload = load_json(path)
    require_keys(
        payload,
        ("schema_version", "sample_rate", "speakers", "receivers", "stimuli", "layouts"),
        "manifest",
    )
    root = path.parent.resolve()
    sample_rate = int(payload["sample_rate"])
    if sample_rate != 48000:
        raise BusinessError(
            E_LAYOUT_INVALID,
            "only 48000 Hz synthetic measurements are accepted",
            details={"sample_rate": sample_rate},
        )
    speakers = tuple(str(item) for item in payload["speakers"])
    receivers = tuple(str(item) for item in payload["receivers"])
    if not speakers or not receivers:
        raise BusinessError(E_LAYOUT_INVALID, "speakers and receivers must be non-empty")
    stimuli = {
        str(source): _resolve(root, str(relative))
        for source, relative in dict(payload["stimuli"]).items()
    }
    if set(stimuli) != set(speakers):
        raise BusinessError(
            E_LAYOUT_INVALID,
            "stimulus map must cover every source speaker",
            details={"speakers": list(speakers), "stimuli": sorted(stimuli)},
        )
    raw_main_receivers = payload.get("main_receivers")
    if raw_main_receivers is None:
        if any(speaker not in receivers for speaker in speakers):
            raise BusinessError(
                E_LAYOUT_INVALID,
                "main_receivers is required when source and pickup ids differ",
                details={
                    "speakers": list(speakers),
                    "receivers": list(receivers),
                },
            )
        main_receivers = {source: source for source in speakers}
    else:
        if not isinstance(raw_main_receivers, dict):
            raise BusinessError(
                E_LAYOUT_INVALID, "main_receivers must be an object"
            )
        main_receivers = {
            str(source): str(receiver)
            for source, receiver in raw_main_receivers.items()
        }
        if set(main_receivers) != set(speakers):
            raise BusinessError(
                E_LAYOUT_INVALID,
                "main_receivers must map every source speaker",
                details={
                    "speakers": list(speakers),
                    "mapped": sorted(main_receivers),
                },
            )
        invalid = sorted(
            receiver
            for receiver in main_receivers.values()
            if receiver not in receivers
        )
        if invalid:
            raise BusinessError(
                E_LAYOUT_INVALID,
                "main_receivers references undeclared pickup ids",
                details={"invalid_receivers": invalid},
            )
    layouts = payload["layouts"]
    if not isinstance(layouts, dict) or layout not in layouts:
        raise BusinessError(
            E_LAYOUT_INVALID,
            "selected measurement layout is not described by manifest",
            details={"layout": layout, "available": sorted(layouts or {})},
        )
    selected = layouts[layout]
    require_keys(selected, ("pairs",), f"manifest.layouts.{layout}")
    raw_pairs = selected["pairs"]
    if not isinstance(raw_pairs, list) or not raw_pairs:
        raise BusinessError(
            E_LAYOUT_INVALID,
            "selected measurement layout has no pairs",
        )
    pairs: List[PairMeasurement] = []
    seen = set()
    for index, raw in enumerate(raw_pairs):
        require_keys(
            raw,
            ("source", "receiver", "path", "channel"),
            f"manifest.layouts.{layout}.pairs[{index}]",
        )
        source = str(raw["source"])
        receiver = str(raw["receiver"])
        if source not in speakers or receiver not in receivers:
            raise BusinessError(
                E_PAIR_MISSING,
                "pair references an undeclared speaker or receiver",
                source=source,
                receiver=receiver,
            )
        key = (source, receiver)
        if key in seen:
            raise BusinessError(
                E_LAYOUT_INVALID,
                "duplicate source-receiver pair",
                source=source,
                receiver=receiver,
            )
        seen.add(key)
        relative = str(raw["path"])
        resolved = _resolve(root, relative)
        try:
            info = sf.info(str(resolved))
        except Exception as exc:
            raise BusinessError(
                E_PAIR_MISSING,
                f"cannot open component WAV: {exc}",
                source=source,
                receiver=receiver,
                details={"path": relative},
            ) from exc
        channel = int(raw["channel"])
        if layout == "mono" and info.channels != 1:
            raise BusinessError(
                E_LAYOUT_INVALID,
                "mono pair WAV must contain one channel",
                source=source,
                receiver=receiver,
                details={"channels": info.channels},
            )
        if layout == "5.1" and info.channels != 6:
            raise BusinessError(
                E_LAYOUT_INVALID,
                "5.1 source capture must contain six pickup channels",
                source=source,
                receiver=receiver,
                details={"channels": info.channels},
            )
        if not 0 <= channel < info.channels:
            raise BusinessError(
                E_LAYOUT_INVALID,
                "pair channel is outside WAV channel count",
                source=source,
                receiver=receiver,
                details={"channel": channel, "channels": info.channels},
            )
        pairs.append(
            PairMeasurement(
                source=source,
                receiver=receiver,
                path=resolved,
                path_label=relative,
                channel=channel,
                layout=layout,
                file_channels=info.channels,
            )
        )
    expected_pairs = {(source, receiver) for source in speakers for receiver in receivers}
    present = {(pair.source, pair.receiver) for pair in pairs}
    missing = sorted(expected_pairs - present)
    if missing:
        source, receiver = missing[0]
        raise BusinessError(
            E_PAIR_MISSING,
            "measurement matrix is incomplete",
            source=source,
            receiver=receiver,
            details={"missing_pairs": [f"{left}|{right}" for left, right in missing]},
        )
    description = str(selected.get("description", layout))
    return MeasurementManifest(
        root=root,
        sample_rate=sample_rate,
        speakers=speakers,
        receivers=receivers,
        main_receivers=main_receivers,
        stimuli=stimuli,
        pairs=tuple(pairs),
        layout=layout,
        layout_description=description,
        digest=file_hash(path),
    )


def component_content_hash(manifest: MeasurementManifest, pair: PairMeasurement) -> str:
    return file_hash(pair.path)
