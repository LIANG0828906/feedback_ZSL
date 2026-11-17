import soundfile as sf

from applause_master.bands import compute_ratios
from applause_master.detect import detect_events
from applause_master.pipeline import detection_accuracy, read_microphones
from applause_master.synth import derive_schedule


def test_synthesis_is_byte_identical(demo_workspace):
    project = demo_workspace["project"]
    before = {}
    for mic in project["microphones"]:
        with open(mic["file"], "rb") as handle:
            before[mic["name"]] = handle.read()
    # Re-derive and re-render from the same seed.
    from applause_master.synth import generate_project_audio
    generate_project_audio(project)
    for mic in project["microphones"]:
        with open(mic["file"], "rb") as handle:
            assert handle.read() == before[mic["name"]]


def test_schedule_recomputes_from_seed(demo_workspace):
    project = demo_workspace["project"]
    truth = demo_workspace["truth"]
    again = derive_schedule(project["seed"], project["duration_seconds"],
                            project["sample_rate"], project["microphones"])
    assert again["events"] == truth["events"]
    assert again["event_count"] == truth["event_count"]


def test_event_count_and_boundary_rmse(demo_workspace):
    project = demo_workspace["project"]
    signals, _ = read_microphones(project)
    events = detect_events(signals, project["sample_rate"], project["detection_threshold"])
    truth = demo_workspace["truth"]
    assert abs(len(events) - truth["event_count"]) <= 1
    rmse = detection_accuracy(events, truth, project["sample_rate"])
    assert rmse <= 20.0


def test_detection_is_deterministic(demo_workspace):
    project = demo_workspace["project"]
    signals, _ = read_microphones(project)
    first = detect_events(signals, project["sample_rate"], project["detection_threshold"])
    second = detect_events(signals, project["sample_rate"], project["detection_threshold"])
    assert first == second


def test_footsteps_do_not_create_events(demo_workspace):
    project = demo_workspace["project"]
    signals, _ = read_microphones(project)
    events = detect_events(signals, project["sample_rate"], project["detection_threshold"])
    truth = demo_workspace["truth"]
    # Footstep mics exist yet the global event list still matches applause only.
    assert truth["footstep_mics"], "demo must include footstep transients"
    assert len(events) == truth["event_count"]


def test_band_ratios_sum_to_one_and_are_order_independent(demo_workspace):
    project = demo_workspace["project"]
    signals, _ = read_microphones(project)
    events = detect_events(signals, project["sample_rate"], project["detection_threshold"])
    names = [mic["name"] for mic in project["microphones"]]
    table, _, _ = compute_ratios(events, names, project["freq_bands"], signals,
                                 project["sample_rate"], project["energy_gate"])
    for entry in table:
        for band in entry["bands"]:
            assert abs(sum(band["ratios"].values()) - 1.0) <= 1e-6

    shuffled = list(reversed(names))
    table_2, _, _ = compute_ratios(events, shuffled, project["freq_bands"], signals,
                                   project["sample_rate"], project["energy_gate"])
    for entry_a, entry_b in zip(table, table_2):
        for band_a, band_b in zip(entry_a["bands"], entry_b["bands"]):
            assert band_a["ratios"] == band_b["ratios"]


def test_energy_gate_excludes_weak_microphones(demo_workspace):
    project = demo_workspace["project"]
    signals, _ = read_microphones(project)
    events = detect_events(signals, project["sample_rate"], project["detection_threshold"])
    names = [mic["name"] for mic in project["microphones"]]
    strict_gate = 0.5
    table, _, _ = compute_ratios(events, names, project["freq_bands"], signals,
                                 project["sample_rate"], strict_gate)
    gated = [band for entry in table for band in entry["bands"] if band["gated_microphones"]]
    assert gated, "a strict gate must exclude at least some microphones"
    for band in gated:
        for name in band["gated_microphones"]:
            assert name in band["ratios"]
        participating = [n for n in names if n not in band["gated_microphones"]]
        assert abs(sum(band["ratios"][n] for n in participating) - 1.0) <= 1e-6


def test_all_mics_share_same_sample_count(demo_workspace):
    project = demo_workspace["project"]
    counts = {sf.info(mic["file"]).frames for mic in project["microphones"]}
    assert counts == {int(project["duration_seconds"] * project["sample_rate"])}


def test_report_carries_ground_truth_provenance(demo_workspace):
    truth = demo_workspace["truth"]
    assert "provenance" in truth
    assert truth["events"][0]["pulse_times"]
