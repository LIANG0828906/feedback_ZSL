#!/usr/bin/env python3
"""Test helper: hard-exit after N durable commits, without shutdown cleanup."""

from __future__ import annotations

import argparse
import os
from pathlib import Path

from audio_repair.audio_io import load_audio
from audio_repair.config import SessionConfig, default_config
from audio_repair.engine import RepairSession


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("audio", type=Path)
    parser.add_argument("checkpoint", type=Path)
    parser.add_argument("segment_samples", type=int)
    parser.add_argument("after_commits", type=int)
    args = parser.parse_args()
    audio = load_audio(args.audio)
    profile = "speech" if "speech" in args.audio.name else "music"
    config = default_config(profile)
    session_config = SessionConfig(
        segment_samples=args.segment_samples,
        max_workers=1,
        max_buffer_bytes=64 * 1024 * 1024,
    )

    def callback(event: object) -> None:
        if getattr(event, "committed", 0) >= args.after_commits:
            os._exit(27)

    session = RepairSession(
        audio,
        config,
        session_config,
        args.checkpoint,
        clear_checkpoint=True,
        progress_callback=callback,
    )
    session.run()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

