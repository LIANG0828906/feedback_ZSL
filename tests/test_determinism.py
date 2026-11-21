from spectral_studio import demo, samplegen


def test_demo_trace_byte_identical(tmp_path):
    out_a = tmp_path / "a"
    out_b = tmp_path / "b"
    for out in (out_a, out_b):
        (out / "samples").mkdir(parents=True)
        (out / "artifacts").mkdir(parents=True)
    summary_a = demo.run_demo(seed=7, samples_dir=str(out_a / "samples"),
                              artifacts_dir=str(out_a / "artifacts"))
    summary_b = demo.run_demo(seed=7, samples_dir=str(out_b / "samples"),
                              artifacts_dir=str(out_b / "artifacts"))
    assert summary_a["final_chain_hash"] == summary_b["final_chain_hash"]
    assert summary_a["recovery_consistent"]
    with open(summary_a["trace_path"], "rb") as handle:
        trace_a = handle.read()
    with open(summary_b["trace_path"], "rb") as handle:
        trace_b = handle.read()
    assert trace_a == trace_b


def test_sample_generator_deterministic(tmp_path):
    materials_a, baseline_a = samplegen.generate_samples(seed=99)
    materials_b, baseline_b = samplegen.generate_samples(seed=99)
    assert materials_a == materials_b
    assert baseline_a == baseline_b
    kinds = {m["kind"] for m in materials_a}
    assert kinds == {"spectral", "srgb", "display-p3"}
    grids = {len(m["spectral"]["wavelengths_nm"])
             for m in materials_a if m["kind"] == "spectral"}
    assert len(grids) == 3  # multiple distinct wavelength grids
