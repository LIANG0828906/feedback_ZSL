"""End-to-end tests for the tactical pathfinding component.

Run with:  python -m unittest discover -s tests -v
"""

import copy
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from tacpath import advance, fp, mapgen, mapio, pipeline, search, smooth
from tacpath.checkpoint import CheckpointStore
from tacpath.errors import (E_BOUND, E_CANCELLED, E_DECODE, E_DIFF, E_HEUR,
                            E_PARA, E_POINT, E_SCHEMA, E_SMOOTH, TacError)

MAPS_DIR = os.path.join(os.path.dirname(os.path.dirname(
    os.path.abspath(__file__))), "maps")


def ensure_maps():
    if not os.path.exists(os.path.join(MAPS_DIR, "manifest.json")):
        mapgen.generate_all(MAPS_DIR)


def load_repo():
    ensure_maps()
    repo = mapio.MapRepository()
    repo.load_all(os.path.join(MAPS_DIR, "manifest.json"))
    return repo


def tmp_store():
    return CheckpointStore(os.path.join(tempfile.mkdtemp(), "ck.json"))


class TestMapIO(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        ensure_maps()
        with open(os.path.join(MAPS_DIR, "map_0.json")) as fh:
            cls.raw = json.load(fh)

    def expect_code(self, code, fn):
        with self.assertRaises(TacError) as ctx:
            fn()
        self.assertEqual(ctx.exception.code, code)
        return ctx.exception

    def test_valid_map_loads(self):
        mapio.validate_map(copy.deepcopy(self.raw))

    def test_missing_field_e_schema(self):
        raw = copy.deepcopy(self.raw)
        del raw["costs"]
        exc = self.expect_code(E_SCHEMA, lambda: mapio.validate_map(raw))
        self.assertEqual(exc.details["field"], "costs")

    def test_dimension_mismatch_e_schema(self):
        raw = copy.deepcopy(self.raw)
        raw["costs"] = raw["costs"][:-1]
        exc = self.expect_code(E_SCHEMA, lambda: mapio.validate_map(raw))
        self.assertIn("costs", exc.details["field"])

    def test_non_integer_cost_e_schema(self):
        raw = copy.deepcopy(self.raw)
        raw["costs"][0][0] = 1.5
        self.expect_code(E_SCHEMA, lambda: mapio.validate_map(raw))

    def test_cost_out_of_range_e_schema(self):
        raw = copy.deepcopy(self.raw)
        raw["costs"][0][0] = fp.MAX_CELL_COST + 1
        self.expect_code(E_SCHEMA, lambda: mapio.validate_map(raw))

    def test_impassable_marker_e_schema(self):
        raw = copy.deepcopy(self.raw)
        for y in range(raw["height"]):
            for x in range(raw["width"]):
                if raw["terrain"][y][x] == 3:
                    raw["costs"][y][x] = 5
                    break
            else:
                continue
            break
        exc = self.expect_code(E_SCHEMA, lambda: mapio.validate_map(raw))
        self.assertIn("costs[", exc.details["field"])

    def test_bad_point_e_point_and_repo_untouched(self):
        raw = copy.deepcopy(self.raw)
        raw["pairs"][0]["start"] = [-1, 0]
        exc = self.expect_code(E_POINT, lambda: mapio.validate_map(raw))
        self.assertIn("start", exc.details["role"])
        repo = mapio.MapRepository()
        repo.load_all(os.path.join(MAPS_DIR, "manifest.json"))
        before = dict(repo.maps)
        with self.assertRaises(TacError):
            mapio.validate_map(raw)
        self.assertEqual(repo.maps, before)  # existing state untouched

    def test_corrupted_file_e_decode(self):
        d = tempfile.mkdtemp()
        p = os.path.join(d, "broken.json")
        with open(p, "w") as fh:
            fh.write('{"id": "x", "costs": [[1, 2],')
        self.expect_code(E_DECODE, lambda: mapio.load_map(p))


class TestSearch(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.repo = load_repo()

    def test_weight_over_one_e_heur(self):
        m = self.repo.maps["map_0"]
        start, goal = m.pairs[0]
        with self.assertRaises(TacError) as ctx:
            search.astar(m.costs, start, goal, weight_num=2, weight_den=1)
        self.assertEqual(ctx.exception.code, E_HEUR)

    def test_truth_zero_error_all_maps(self):
        for m in self.repo.maps.values():
            for pi, (start, goal) in enumerate(m.pairs):
                res = search.astar(m.costs, start, goal,
                                   neighborhood=m.neighborhood)
                truth = m.truth[str(pi)]
                self.assertEqual(res.cost_fp, truth["cost_fp"])
                self.assertEqual(len(res.path), truth["path_len"])
                corners = [list(c)
                           for c in smooth.extract_raw_corners(res.path)]
                self.assertEqual(corners, truth["corners"])

    def test_probe_costs_zero_error(self):
        for m in self.repo.maps.values():
            for pi, (start, goal) in enumerate(m.pairs):
                g = search.dijkstra_costs(m.costs, start, m.neighborhood)
                for key, vals in m.truth[str(pi)]["probes"].items():
                    x, y = (int(v) for v in key.split(","))
                    self.assertEqual(g.get((x, y)), vals["from_start"])

    def test_four_neighborhood_supported(self):
        m = self.repo.maps["map_0"]
        start, goal = m.pairs[0]
        res = search.astar(m.costs, start, goal, neighborhood=4)
        self.assertGreater(res.cost_fp, 0)


class TestSmooth(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.repo = load_repo()

    def test_tolerance_monotonicity(self):
        m = self.repo.maps["map_1"]
        start, goal = m.pairs[0]
        path = search.astar(m.costs, start, goal).path
        counts = []
        for tol in (0, 250, 500, 1000, 2000, 5000):
            counts.append(len(smooth.greedy_smooth(m.costs, path, tol)))
        for a, b in zip(counts, counts[1:]):
            self.assertGreaterEqual(a, b)  # tightening never removes corners

    def test_smoothed_never_crosses_impassable(self):
        for m in self.repo.maps.values():
            start, goal = m.pairs[0]
            path = search.astar(m.costs, start, goal).path
            corners = smooth.greedy_smooth(m.costs, path, 500)
            for a, b in zip(corners, corners[1:]):
                self.assertTrue(smooth.los_passable(m.costs, a, b))

    def test_smoothed_cost_bounds(self):
        for m in self.repo.maps.values():
            start, goal = m.pairs[0]
            res = search.astar(m.costs, start, goal)
            corners = smooth.greedy_smooth(m.costs, res.path, 500)
            cost, cells = smooth.polyline_cost_fp(m.costs, corners)
            bound = smooth.smooth_cost_bound_fp(cells, m.max_cell_cost,
                                                res.cost_fp)
            self.assertGreaterEqual(cost, res.cost_fp)
            self.assertLessEqual(cost - res.cost_fp, bound)

    def test_e_smooth_on_invariant_violation(self):
        with self.assertRaises(TacError) as ctx:
            smooth.validate_smoothing(5, 10, 100)
        self.assertEqual(ctx.exception.code, E_SMOOTH)
        with self.assertRaises(TacError) as ctx:
            smooth.validate_smoothing(1000, 10, 100)
        self.assertEqual(ctx.exception.code, E_SMOOTH)

    def test_dual_readout(self):
        corners = [(3, 4)]
        self.assertEqual(smooth.corners_world(corners), [(3500, 4500)])


class TestAdvance(unittest.TestCase):
    WAYPOINTS = [(500, 500), (10500, 500), (10500, 8500)]

    def test_e_para(self):
        with self.assertRaises(TacError) as ctx:
            advance.simulate(self.WAYPOINTS, 0, 100)
        self.assertEqual(ctx.exception.code, E_PARA)
        with self.assertRaises(TacError) as ctx:
            advance.simulate(self.WAYPOINTS, 100, 0)
        self.assertEqual(ctx.exception.code, E_PARA)
        with self.assertRaises(TacError) as ctx:
            advance.simulate(self.WAYPOINTS, fp.MAX_STEP_FP + 1, 100)
        self.assertEqual(ctx.exception.code, E_PARA)

    def test_stays_on_polyline_no_segment_skip(self):
        traj, state, _ = advance.simulate(self.WAYPOINTS, 137, 200)
        self.assertTrue(traj)
        segs = [t[4] for t in traj]
        self.assertEqual(segs, sorted(segs))  # never jumps backwards
        for a, b in zip(segs, segs[1:]):
            self.assertLessEqual(b - a, 1)    # never skips a segment
        for t in traj:
            self.assertTrue(
                advance.on_polyline_check(self.WAYPOINTS, t[1], t[2], t[4]))

    def test_no_instant_turn_at_corner(self):
        traj, _, _ = advance.simulate(self.WAYPOINTS, 200, 150)
        for a, b in zip(traj, traj[1:]):
            self.assertLessEqual(abs(fp.wrap_mrad(b[3] - a[3])), 150)

    def test_frame_rate_bound(self):
        m = load_repo().maps["map_2"]
        start, goal = m.pairs[0]
        path = search.astar(m.costs, start, goal).path
        corners = smooth.greedy_smooth(m.costs, path, 0)
        waypoints = smooth.corners_world(corners)
        fr = pipeline.frame_rate_deviation(waypoints, 200, 300)
        self.assertLessEqual(fr["max_deviation_fp"], fr["bound_fp"])


class TestThreePaths(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.repo = load_repo()

    def test_realtime_resume_recompute_identical(self):
        m = self.repo.maps["map_1"]
        params = dict(pipeline.DEFAULT_PARAMS)
        d = tempfile.mkdtemp()

        out_rt = pipeline.run_pipeline(
            m, 0, params, CheckpointStore(os.path.join(d, "a.json")),
            "realtime")

        resume_store = CheckpointStore(os.path.join(d, "b.json"))
        with self.assertRaises(TacError) as ctx:
            pipeline.run_pipeline(m, 0, params, resume_store, "realtime",
                                  cancel_after_segment=1)
        self.assertEqual(ctx.exception.code, E_CANCELLED)
        out_rs = pipeline.run_pipeline(m, 0, params, resume_store, "resume")

        out_rc = pipeline.run_pipeline(
            m, 0, params, CheckpointStore(os.path.join(d, "c.json")),
            "recompute")

        self.assertTrue(pipeline.compare_outputs(out_rt, out_rs))
        self.assertTrue(pipeline.compare_outputs(out_rt, out_rc))

    def test_checkpoint_version_mismatch_full_recompute(self):
        m = self.repo.maps["map_0"]
        params = dict(pipeline.DEFAULT_PARAMS)
        store = tmp_store()
        pipeline.run_pipeline(m, 0, params, store, "realtime")
        self.assertTrue(os.path.exists(store.path))
        changed = dict(params, step_fp=params["step_fp"] + 1)
        pipeline.run_pipeline(m, 0, changed, store, "resume")
        with open(store.path) as fh:
            data = json.load(fh)
        from tacpath.checkpoint import param_version
        self.assertEqual(data["param_version"], param_version(changed))

    def test_checkpoint_map_hash_mismatch_discarded(self):
        m = self.repo.maps["map_0"]
        params = dict(pipeline.DEFAULT_PARAMS)
        store = tmp_store()
        pipeline.run_pipeline(m, 0, params, store, "realtime")
        with open(store.path) as fh:
            data = json.load(fh)
        data["map_hash"] = "0" * 64
        with open(store.path, "w") as fh:
            json.dump(data, fh)
        self.assertIsNone(store.load("1" * 64, data["param_version"]))
        self.assertFalse(os.path.exists(store.path))

    def test_segment_failure_retry_keeps_completed(self):
        m = self.repo.maps["map_0"]
        params = dict(pipeline.DEFAULT_PARAMS)
        store = tmp_store()
        calls = {"n": 0}
        saved = []
        orig_save = store.save
        store.save = lambda snap: (saved.append(
            snap["advance"]["done_segments"]), orig_save(snap))

        def hook(seg):
            if seg == 1 and calls["n"] == 0:
                calls["n"] += 1
                raise TacError("E_SEGMENT", "injected failure")

        out = pipeline.run_pipeline(m, 0, params, store, "realtime",
                                    fault_hook=hook)
        self.assertEqual(calls["n"], 1)          # failed once, retried
        self.assertEqual(saved, sorted(saved))   # segments completed in order
        self.assertGreater(len(out.trajectory), 0)

    def test_e_diff_locates_first_mismatch(self):
        m = self.repo.maps["map_0"]
        params = dict(pipeline.DEFAULT_PARAMS)
        out_a = pipeline.run_pipeline(m, 0, params, tmp_store(), "realtime")
        out_b = pipeline.run_pipeline(m, 0, params, tmp_store(), "realtime")
        t = out_b.trajectory[5]
        out_b.trajectory[5] = (t[0], t[1] + 1, t[2], t[3], t[4], t[5])
        with self.assertRaises(TacError) as ctx:
            pipeline.compare_outputs(out_a, out_b)
        self.assertEqual(ctx.exception.code, E_DIFF)
        self.assertEqual(ctx.exception.details["tick"],
                         out_a.trajectory[5][0])
        self.assertIn("segment", ctx.exception.details)
        rep = pipeline.replay_tick({"a": out_a, "b": out_b},
                                   out_a.trajectory[5][0])
        self.assertNotEqual(rep["a"], rep["b"])


class TestReport(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.repo = load_repo()

    def test_report_four_stats_and_zero_violations(self):
        m = self.repo.maps["map_0"]
        out = pipeline.run_pipeline(m, 0, dict(pipeline.DEFAULT_PARAMS),
                                    tmp_store(), "realtime")
        stats = out.report["statistics"]
        for key in ("cost_accumulation", "heuristic_deviation",
                    "smoothing_cost", "advance_quantization"):
            self.assertIn(key, stats)
            self.assertIn("cumulative_bound", stats[key])
        self.assertEqual(out.report["violations"], [])
        self.assertEqual(stats["cost_accumulation"]["max_error_fp"], 0)
        self.assertEqual(stats["heuristic_deviation"]
                         ["max_overestimate_fp"], 0)

    def test_e_bound_on_tampered_truth(self):
        m = self.repo.maps["map_0"]
        raw = copy.deepcopy(m.raw)
        key = next(iter(raw["truth"]["0"]["probes"]))
        raw["truth"]["0"]["probes"][key]["from_start"] += 1
        m2 = mapio.MapData(raw, m.source_path)
        with self.assertRaises(TacError) as ctx:
            pipeline.run_pipeline(m2, 0, dict(pipeline.DEFAULT_PARAMS),
                                  tmp_store(), "realtime")
        self.assertEqual(ctx.exception.code, E_BOUND)
        viol = ctx.exception.details["violations"][0]
        self.assertEqual(viol["stat"], "cost_accumulation")
        self.assertIn("cell", viol)


if __name__ == "__main__":
    unittest.main()
