from __future__ import annotations

from dataclasses import replace
import threading

from lcft.engine import incremental_layout, layout_track
from lcft.loader import load_bundle
from lcft.params import LayoutParams
from lcft.version_store import VersionStore


def test_incremental_reuses_unaffected_rows_and_matches_full(
    sample_bundle_path
):
    loaded = load_bundle(sample_bundle_path)
    previous = layout_track(
        "speech-en",
        "en",
        loaded.normalized.tracks["en"],
        loaded.normalized.descriptions,
        loaded.normalized,
        LayoutParams(),
        published_version=1,
        param_version=1,
    )
    speech = list(loaded.normalized.tracks["en"])
    speech[-1] = replace(speech[-1], text="one two")
    incremental, stats = incremental_layout(
        previous,
        speech,
        loaded.normalized.descriptions,
        loaded.normalized,
        LayoutParams(),
        published_version=2,
        param_version=2,
    )
    full = layout_track(
        "speech-en",
        "en",
        speech,
        loaded.normalized.descriptions,
        loaded.normalized,
        LayoutParams(),
        published_version=2,
        param_version=2,
    )
    assert incremental["track_hash"] == full["track_hash"]
    assert stats["conservative"] is False
    assert stats["reused_count"] > 0
    assert previous["sentences"][0]["content_hash"] == incremental["sentences"][0]["content_hash"]
    assert previous["sentences"][0] == incremental["sentences"][0]


def test_incremental_description_edit_matches_full(sample_bundle_path):
    loaded = load_bundle(sample_bundle_path)
    previous = layout_track(
        "speech-en",
        "en",
        loaded.normalized.tracks["en"],
        loaded.normalized.descriptions,
        loaded.normalized,
        LayoutParams(),
        published_version=1,
        param_version=1,
    )
    descriptions = list(loaded.normalized.descriptions)
    descriptions[0] = replace(descriptions[0], text="a b")
    incremental, stats = incremental_layout(
        previous,
        loaded.normalized.tracks["en"],
        descriptions,
        loaded.normalized,
        LayoutParams(),
        published_version=2,
        param_version=2,
    )
    full = layout_track(
        "speech-en",
        "en",
        loaded.normalized.tracks["en"],
        descriptions,
        loaded.normalized,
        LayoutParams(),
        published_version=2,
        param_version=2,
    )
    assert incremental["track_hash"] == full["track_hash"]
    assert stats["reused_count"] > 0
    assert stats["affected_ids"]


def test_version_store_preview_is_always_a_complete_version(sample_bundle_path):
    loaded = load_bundle(sample_bundle_path)
    report = layout_track(
        "speech-en",
        "en",
        loaded.normalized.tracks["en"],
        (),
        loaded.normalized,
        LayoutParams(),
        published_version=1,
    )
    store = VersionStore(report)
    stop = threading.Event()
    observed: list[tuple[int, int, int]] = []

    def publisher():
        for version in range(2, 16):
            next_report = dict(report)
            next_report["published_version"] = version
            next_report["track_hash"] = f"{version:064x}"
            store.publish(next_report)
        stop.set()

    def reader():
        while not stop.is_set():
            preview = store.preview()
            observed.append(
                (
                    preview["published_version"],
                    len(preview["sentences"]),
                    len(preview["descriptions"]),
                )
            )

    reader_thread = threading.Thread(target=reader)
    publisher_thread = threading.Thread(target=publisher)
    reader_thread.start()
    publisher_thread.start()
    publisher_thread.join()
    reader_thread.join()
    assert observed
    assert all(sentences == len(report["sentences"]) for _, sentences, _ in observed)
    assert all(descriptions == len(report["descriptions"]) for _, _, descriptions in observed)

