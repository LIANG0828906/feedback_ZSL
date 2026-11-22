from scoreforge.playback import PlaybackEngine


def test_no_drift_across_start_points(loaded):
    score, _ = loaded
    engine = PlaybackEngine(score)
    engine.play(from_tick=0)
    t0 = {e["event_id"]: e["abs_ms"] for e in engine.timeline}
    for start in (120, 960, 4800, score.total_ticks // 2):
        engine.pause()
        engine.play(from_tick=start)
        engine.advance(250.0)
        engine.pause()
        t1 = {e["event_id"]: e["abs_ms"] for e in engine.timeline}
        assert t0 == t1


def test_playback_does_not_mutate_model(loaded):
    score, _ = loaded
    before = score.to_data()
    engine = PlaybackEngine(score)
    engine.play(from_tick=480)
    engine.advance(1000.0)
    engine.state_at_tick(score.part_ids[0], 960)
    engine.events_between(0, 10 ** 6)
    assert score.to_data() == before


def test_events_between_window(loaded):
    score, _ = loaded
    engine = PlaybackEngine(score)
    all_events = engine.events_between(0, 10 ** 9)
    assert len(all_events) == len(score.events)
    if all_events:
        first_ms = all_events[0]["abs_ms"]
        windowed = engine.events_between(0, first_ms + 0.5)
        assert windowed and all(e["abs_ms"] <= first_ms + 0.5 for e in windowed)


def test_pause_and_position(loaded):
    score, _ = loaded
    engine = PlaybackEngine(score)
    engine.play(from_tick=960)
    expected_ms = score.tempo_map.ms_at_tick(960)
    assert abs(engine.position_ms - expected_ms) < 1e-9
    engine.advance(100.0)
    engine.pause()
    pos = engine.position_ms
    engine.advance(100.0)
    assert engine.position_ms == pos  # 暂停后不再推进
