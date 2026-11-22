"""Interactive pygame synchronisation demo.

Shows the authoritative state next to the selected client's visible state,
marks every rendered frame as PREDICTED or AUTHORITATIVE, plots rollback
count and correction magnitude, and lets the user adjust the latency tier,
playback speed, inject inputs and trigger a replay.

Committed checkpoint windows are never silently rewritten: a rerun whose
recomputed window hash differs from the committed hash is rejected and
flagged instead of applied.
"""

from __future__ import annotations

import json
import math
import os
import tempfile

from . import engine, runner
from .checkpoint import CheckpointStore
from .schema import load_sample

PLAYER_COLORS = [
    (235, 87, 87), (86, 156, 214), (106, 190, 105), (220, 180, 80),
    (180, 130, 220), (90, 200, 200),
]
BG = (24, 26, 32)
PANEL = (36, 39, 48)
TEXT = (210, 214, 222)
WARN = (235, 190, 70)
GOOD = (110, 200, 120)


class DemoApp:
    def __init__(self, sample_dir="sample", headless=False):
        if headless:
            os.environ.setdefault("SDL_VIDEODRIVER", "dummy")
        import pygame  # imported lazily so the headless env var applies first

        self.pygame = pygame
        pygame.init()
        self.screen = pygame.display.set_mode((1024, 640))
        pygame.display.set_caption("syncdemo - state sync verification")
        self.font = pygame.font.Font(None, 20)
        self.big_font = pygame.font.Font(None, 34)
        self.clock = pygame.time.Clock()

        self.sample = load_sample(sample_dir)
        self.ecfg = engine.EngineConfig()
        self.rcfg = runner.RunConfig()
        self.latency_offset = {c["id"]: 0 for c in self.sample.clients}
        self.extra_inputs = {}
        self.committed_hashes = {}      # played & locked window -> auth hash
        self._result_hashes = {}        # candidate hashes from latest rerun
        self._played_tick = 0
        self.alert = ""
        self.selected = 0
        self.speed = 10.0               # ticks per second
        self.paused = False
        self.tick_pos = 0.0
        self.result = None
        self._rerun()

    def _rerun(self):
        """Recompute the match; never overwrite committed windows silently."""
        meta = self.sample.meta
        with tempfile.TemporaryDirectory() as tmp:
            store = CheckpointStore(tmp, seed=meta["seed"],
                                    input_fingerprint=meta["input_fingerprint"],
                                    algo_version=meta["algo_version"])
            result = runner.run_realtime(
                self.sample, self.rcfg, self.ecfg, store=store,
                extra_inputs=self.extra_inputs,
                latency_offset=self.latency_offset,
            )
            new_hashes = {}
            for fname in sorted(os.listdir(tmp)):
                if fname.startswith("window_"):
                    record = json.loads((store.path / fname).read_text())
                    # immutability is enforced on the authoritative core;
                    # client views legitimately change with the latency tier
                    new_hashes[record["window"]] = \
                        record["snapshot"]["auth_hash"]
        for window, h in new_hashes.items():
            if window in self.committed_hashes and \
                    self.committed_hashes[window] != h:
                self.alert = (f"rejected: committed window {window} would "
                              f"change (immutable)")
                return
        self._result_hashes = new_hashes
        self.alert = ""
        self.result = result
        max_tick = len(result.states) - 1
        self.tick_pos = min(self.tick_pos, float(max_tick))

    def _commit_played_windows(self):
        """Lock windows the playback has fully passed."""
        self._played_tick = max(self._played_tick, int(self.tick_pos))
        for window, h in self._result_hashes.items():
            if window * self.rcfg.window_ticks <= self._played_tick:
                self.committed_hashes.setdefault(window, h)

    def _inject_input(self):
        tick = int(self.tick_pos) + 1
        committed_upto = (max(self.committed_hashes) * self.rcfg.window_ticks
                          if self.committed_hashes else 0)
        if tick <= committed_upto:
            self.alert = (f"input injection rejected: tick {tick} already "
                          f"committed (window immutable)")
            return
        client = self.sample.clients[self.selected]
        uid = next(u["id"] for u in self.sample.units
                   if u["player"] == client["player"])
        self.extra_inputs.setdefault(tick, []).append(
            {"unit": uid, "type": "gather"})
        self._rerun()
        if not self.alert:
            self.alert = f"injected gather for {uid} at tick {tick}"

    def _handle_key(self, key):
        pg = self.pygame
        clients = self.sample.clients
        cid = clients[self.selected]["id"]
        if key == pg.K_SPACE:
            self.paused = not self.paused
        elif key == pg.K_TAB:
            self.selected = (self.selected + 1) % len(clients)
        elif key == pg.K_UP:
            self.latency_offset[cid] += 1
            self._rerun()
        elif key == pg.K_DOWN:
            self.latency_offset[cid] = max(0, self.latency_offset[cid] - 1)
            self._rerun()
        elif key in (pg.K_PLUS, pg.K_EQUALS, pg.K_KP_PLUS):
            self.speed = min(60.0, self.speed * 1.5)
        elif key in (pg.K_MINUS, pg.K_KP_MINUS):
            self.speed = max(1.0, self.speed / 1.5)
        elif key == pg.K_i:
            self._inject_input()
        elif key == pg.K_r:
            self.extra_inputs = {}
            self.tick_pos = 0.0
            self._rerun()
            if not self.alert:
                self.alert = "replayed from tick 0; committed windows verified"
        elif key == pg.K_LEFT:
            self.tick_pos = max(0.0, self.tick_pos - 1)
        elif key == pg.K_RIGHT:
            self.tick_pos = min(len(self.result.states) - 1.0, self.tick_pos + 1)

    def _draw_map(self, origin, size, state, title):
        pg = self.pygame
        ox, oy = origin
        pg.draw.rect(self.screen, PANEL, (ox, oy, size, size))
        pg.draw.rect(self.screen, TEXT, (ox, oy, size, size), 1)
        label = self.font.render(title, True, TEXT)
        self.screen.blit(label, (ox, oy - 22))
        if state is None:
            return
        scale = size / self.ecfg.map_size
        players = sorted({u["player"] for u in state["units"].values()})
        for unit in state["units"].values():
            color = PLAYER_COLORS[players.index(unit["player"])
                                  % len(PLAYER_COLORS)]
            x, y = ox + unit["x"] * scale, oy + unit["y"] * scale
            if not unit["alive"]:
                pg.draw.circle(self.screen, color, (x, y), 7, 1)
                continue
            pg.draw.circle(self.screen, color, (x, y), 7)
            rad = math.radians(unit["facing"])
            pg.draw.line(self.screen, BG, (x, y),
                         (x + 9 * math.cos(rad), y + 9 * math.sin(rad)), 2)
            hp_w = 16 * unit["hp"] / unit["max_hp"]
            pg.draw.rect(self.screen, (90, 40, 40), (x - 8, y - 14, 16, 3))
            pg.draw.rect(self.screen, GOOD, (x - 8, y - 14, hp_w, 3))

    def _draw_curve(self, origin, size, values, title):
        pg = self.pygame
        ox, oy = origin
        w, h = size
        pg.draw.rect(self.screen, PANEL, (ox, oy, w, h))
        pg.draw.rect(self.screen, TEXT, (ox, oy, w, h), 1)
        self.screen.blit(self.font.render(title, True, TEXT), (ox, oy - 22))
        if not values:
            return
        peak = max(values) or 1.0
        points = [
            (ox + i * w / max(1, len(values) - 1),
             oy + h - 4 - (v / peak) * (h - 8))
            for i, v in enumerate(values)
        ]
        if len(points) > 1:
            pg.draw.lines(self.screen, WARN, False, points, 1)

    def _mode_at(self, tick):
        for rec in self.result.frame_log:
            if rec.tick >= tick:
                return rec.mode, rec.reason
        last = self.result.frame_log[-1]
        return last.mode, last.reason

    def _render(self):
        pg = self.pygame
        self.screen.fill(BG)
        result = self.result
        tick = int(self.tick_pos)
        state = result.states[tick]
        cid = self.sample.clients[self.selected]["id"]
        views = result.client_views[cid]
        view = views[min(tick, len(views) - 1)]

        mode, reason = self._mode_at(max(1, tick))
        mode_color = WARN if mode == "predicted" else GOOD
        self.screen.blit(
            self.big_font.render(f"FRAME MODE: {mode.upper()}", True,
                                 mode_color), (20, 12))
        self.screen.blit(self.font.render(f"reason: {reason}", True, TEXT),
                         (420, 20))

        self._draw_map((20, 60), 360, state, f"authoritative @ tick {tick}")
        self._draw_map((420, 60), 360, view,
                       f"client {cid} view "
                       f"(latency +{self.latency_offset[cid]})")

        corrections = result.corrections[cid]
        self._draw_curve((820, 60), (190, 120),
                         [c.total for c in corrections],
                         f"corrections (rollbacks={len(corrections)})")
        settlement = result.settlement["players"]
        lines = [
            f"tick {tick}/{len(result.states) - 1}  speed {self.speed:.1f}t/s"
            f"  {'PAUSED' if self.paused else 'RUN'}  status {result.status}",
            f"client {cid} rollbacks={len(corrections)}"
            f"  committed_windows={len(self.committed_hashes)}",
            "settlement: " + ", ".join(
                f"{p}: res={v['resource']:.0f} hp={v['hp']:.0f}"
                for p, v in sorted(settlement.items())),
            "[space]pause [tab]client [up/dn]latency [+/-]speed "
            "[i]nject [r]eplay [q]uit",
        ]
        if self.alert:
            lines.append(f"! {self.alert}")
        for i, line in enumerate(lines):
            color = WARN if line.startswith("!") else TEXT
            self.screen.blit(self.font.render(line, True, color),
                             (20, 450 + i * 24))
        pg.display.flip()

    def run(self, max_frames=None):
        pg = self.pygame
        running = True
        frames = 0
        while running:
            dt = self.clock.tick(60) / 1000.0
            for event in pg.event.get():
                if event.type == pg.QUIT:
                    running = False
                elif event.type == pg.KEYDOWN:
                    if event.key in (pg.K_q, pg.K_ESCAPE):
                        running = False
                    else:
                        self._handle_key(event.key)
            if not self.paused:
                self.tick_pos = min(len(self.result.states) - 1.0,
                                    self.tick_pos + self.speed * dt)
            self._commit_played_windows()
            self._render()
            frames += 1
            if max_frames is not None and frames >= max_frames:
                running = False
        pg.quit()
