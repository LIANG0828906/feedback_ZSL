"""演奏表达回放引擎.

回放只读已提交内部模型，绝不修改工作副本状态；
事件绝对时间由 tick 时间轴统一换算，切换起播点后同一事件的
绝对时间戳不漂移（时间轴预先一次性计算，播放仅移动读指针）。
"""

from __future__ import annotations

from .model import Score


class PlaybackEngine:
    def __init__(self, score: Score):
        self.score = score
        self.timeline = score.export_timeline()
        self._start_tick = 0
        self._playing = False
        self._position_ms = 0.0

    def play(self, from_tick: int = 0) -> None:
        self._start_tick = max(0, min(int(from_tick), self.score.total_ticks))
        self._position_ms = self.score.tempo_map.ms_at_tick(self._start_tick)
        self._playing = True

    def pause(self) -> None:
        self._playing = False

    def seek(self, tick: int) -> None:
        self.play(tick)
        self._playing = False

    @property
    def playing(self) -> bool:
        return self._playing

    @property
    def position_ms(self) -> float:
        return self._position_ms

    @property
    def position_tick(self) -> int:
        return self.score.tempo_map.tick_at_ms(self._position_ms)

    def advance(self, elapsed_ms: float) -> None:
        """由外部时钟（GUI 定时器）驱动推进；不改变事件时间轴."""
        if self._playing:
            self._position_ms += max(0.0, float(elapsed_ms))

    def events_between(self, ms0: float, ms1: float) -> list[dict]:
        lo, hi = min(ms0, ms1), max(ms0, ms1)
        return [e for e in self.timeline if lo <= e["abs_ms"] < hi]

    def event_abs_ms(self, event_id: str) -> float:
        for e in self.timeline:
            if e["event_id"] == event_id:
                return e["abs_ms"]
        raise KeyError(event_id)

    def state_at_tick(self, part_id: str, tick: int) -> dict:
        """指定拍位处的演奏状态（力度 / 连奏 / 踏板），供界面高亮."""
        dynamics = self.score.dynamics_value_at(part_id, tick)
        legato = 0
        pedal = 0
        for ev in self.score.events.values():
            if ev.part_id != part_id:
                continue
            if self.score.event_abs_tick(ev) <= tick:
                if ev.type == "legato":
                    legato = ev.value
                elif ev.type == "pedal":
                    pedal = ev.value
        return {"dynamics": dynamics, "legato": legato, "pedal": pedal}
