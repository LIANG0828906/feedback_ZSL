"""重放引擎：按玩家缓冲指令，在固定时间步边界以稳定顺序结算。

结算顺序只取决于输入内容（recv_tick / recv_seq / player / client_seq），
不依赖系统时间或全局随机状态；相同初始状态与指令序列下整条哈希链
逐字节一致。
"""

from __future__ import annotations

import hashlib

from .canon import GENESIS_CHAIN, chain_hash, hash_obj
from .errors import CancelledError
from .models import (BASELINE_VERSION, STATE_VERSION, BattleState, Command)


def _normalize_quota(raw: dict, players: list[str], frames: int) -> dict:
    table = {}
    for p in players:
        q = raw.get(p, 0)
        if isinstance(q, list):
            if len(q) < frames:
                raise ValueError(f"quota list for {p} shorter than frames")
            table[p] = [int(v) for v in q[:frames]]
        else:
            table[p] = [int(q)] * frames
    return table


class ReplayEngine:
    def __init__(self, match: dict):
        self.match_id = str(match["match_id"])
        self.frames = int(match["frames"])
        self.tick_ms = int(match.get("tick_ms", 100))
        self.window = int(match.get("compensation_window", 0))
        self.state = BattleState.from_dict(match["initial_state"])
        self.players = self.state.players
        self.quota = _normalize_quota(match.get("quota_per_frame", {}),
                                      self.players, self.frames)
        self.commands = sorted(
            (Command.from_dict(c) for c in match.get("commands", [])),
            key=lambda c: (c.recv_tick, c.recv_seq, c.player, c.client_seq))
        self._inbox_pos = 0
        self.buffers: dict[str, dict[int, Command]] = {
            p: {} for p in self.players}
        self.settled: dict[str, set[int]] = {p: set() for p in self.players}
        self.step_no = 0
        self.chain_tip = GENESIS_CHAIN
        self.hash_chain: list[dict] = []
        self.records: list[dict] = []

    # ---- 指令摄取与结算 -------------------------------------------------

    def _ingest(self, tick: int, events: list[dict]):
        while (self._inbox_pos < len(self.commands)
               and self.commands[self._inbox_pos].recv_tick <= tick):
            cmd = self.commands[self._inbox_pos]
            self._inbox_pos += 1
            if cmd.player not in self.buffers:
                events.append({"type": "E_CMD_INVALID", "cmd": cmd.cmd_id,
                               "reason": "unknown_player"})
                continue
            if cmd.client_seq in self.settled[cmd.player]:
                events.append({"type": "E_CMD_DUP", "cmd": cmd.cmd_id,
                               "player": cmd.player,
                               "recv_seq": cmd.recv_seq})
                continue
            buf = self.buffers[cmd.player]
            if cmd.client_seq in buf:
                events.append({"type": "E_CMD_DUP", "cmd": cmd.cmd_id,
                               "player": cmd.player,
                               "recv_seq": cmd.recv_seq})
                continue
            buf[cmd.client_seq] = cmd

    def _validate(self, cmd: Command) -> str | None:
        """结算前校验指令合法性；返回 None 表示合法，否则为拒绝原因。"""
        if cmd.unit not in self.state.owners:
            return "unknown_unit"
        owner = self.state.owners[cmd.unit]
        idx = self.state.index(cmd.unit)
        if cmd.op in ("move", "stop") and owner != cmd.player:
            return "unit_not_owned"
        if cmd.op == "strike" and owner == cmd.player:
            return "friendly_fire"
        if not bool(self.state.alive[idx]):
            return "target_dead"
        return None

    def _apply(self, cmd: Command):
        idx = self.state.index(cmd.unit)
        if cmd.op == "move":
            self.state.vel[idx] = (cmd.dx, cmd.dy)
        elif cmd.op == "stop":
            self.state.vel[idx] = (0, 0)
        elif cmd.op == "strike":
            self.state.alive[idx] = False
            self.state.vel[idx] = (0, 0)

    def step(self) -> dict:
        t = self.step_no
        if t >= self.frames:
            raise RuntimeError("replay already finished")
        events: list[dict] = []
        self._ingest(t, events)

        # 1) 迟到淘汰：超过声明补偿窗口的指令以 E_CMD_LATE 拒绝
        for player in self.players:
            buf = self.buffers[player]
            for seq in sorted(buf):
                cmd = buf[seq]
                if t - cmd.submit_tick > self.window:
                    events.append({"type": "E_CMD_LATE", "cmd": cmd.cmd_id,
                                   "player": player, "frame": t,
                                   "submit_tick": cmd.submit_tick})
                    del buf[seq]
                    self.settled[player].add(seq)

        # 2) 稳定顺序结算：全体候选按 (recv_seq, player, client_seq) 排序
        pending = [self.buffers[player][seq] for player in self.players
                   for seq in self.buffers[player]]
        pending.sort(key=lambda c: (c.recv_seq, c.player, c.client_seq))

        quota_used = {p: 0 for p in self.players}
        applied: list[str] = []
        for cmd in pending:
            self.buffers[cmd.player].pop(cmd.client_seq)
            self.settled[cmd.player].add(cmd.client_seq)
            reason = self._validate(cmd)
            if reason is not None:
                events.append({"type": "E_CMD_INVALID", "cmd": cmd.cmd_id,
                               "player": cmd.player, "frame": t,
                               "reason": reason})
                continue
            # 3) 配额强制：超额即拒，配额不得在运行时被静默提高
            if quota_used[cmd.player] + cmd.cost > self.quota[cmd.player][t]:
                events.append({"type": "E_QUOTA_EXCEEDED", "cmd": cmd.cmd_id,
                               "player": cmd.player, "frame": t,
                               "cost": cmd.cost,
                               "quota": self.quota[cmd.player][t],
                               "used": quota_used[cmd.player]})
                continue
            quota_used[cmd.player] += cmd.cost
            self.state.quota_used_total[cmd.player] += cmd.cost
            self._apply(cmd)
            applied.append(cmd.cmd_id)
            events.append({"type": "CMD_APPLIED", "cmd": cmd.cmd_id,
                           "player": cmd.player, "op": cmd.op,
                           "frame": t})

        # 4) 权威推进：存活单位按速度积分（整数运算，确定性）
        self.state.pos += self.state.vel * self.state.alive[:, None]

        # 5) 确定性哈希链
        quota_remaining = {p: self.quota[p][t] - quota_used[p]
                           for p in self.players}
        canonical = self.state.to_canonical(quota_remaining)
        state_hash = hash_obj(canonical)
        self.chain_tip = chain_hash(self.chain_tip, state_hash)
        self.hash_chain.append({"state_hash": state_hash,
                                "chain_hash": self.chain_tip})

        record = {
            "frame": t,
            "applied": applied,
            "events": events,
            "quota_used": quota_used,
            "quota_remaining": quota_remaining,
            "buffer_watermark": {p: len(self.buffers[p])
                                 for p in self.players},
            "state_hash": state_hash,
            "chain_hash": self.chain_tip,
        }
        self.records.append(record)
        self.step_no += 1
        return record

    def run(self, cancel_event=None, on_frame=None) -> list[dict]:
        while self.step_no < self.frames:
            if cancel_event is not None and cancel_event.is_set():
                raise CancelledError(self.step_no)
            record = self.step()
            if on_frame is not None:
                on_frame(record)
        return self.records

    # ---- 快照载荷 -------------------------------------------------------

    def snapshot_payload(self) -> dict:
        prefix = "".join(h["state_hash"] for h in self.hash_chain)
        return {
            "version": 1,
            "state_version": STATE_VERSION,
            "match_id": self.match_id,
            "step": self.step_no - 1,
            "state": self.state.to_canonical(
                self.records[-1]["quota_remaining"]),
            "buffer": {p: [self.buffers[p][s].to_dict()
                           for s in sorted(self.buffers[p])]
                       for p in self.players},
            "buffer_watermark": self.records[-1]["buffer_watermark"],
            "settled": {p: sorted(self.settled[p]) for p in self.players},
            "quota_used_total": dict(self.state.quota_used_total),
            "state_hash": self.hash_chain[-1]["state_hash"],
            "prev_chain_hash": (self.hash_chain[-2]["chain_hash"]
                                if len(self.hash_chain) > 1
                                else GENESIS_CHAIN),
            "chain_hash": self.chain_tip,
            "hash_prefix_digest": hashlib.sha256(
                prefix.encode("ascii")).hexdigest(),
        }

    # ---- 恢复 -----------------------------------------------------------

    @classmethod
    def recover(cls, match: dict, store) -> "ReplayEngine":
        """从最近有效快照重建引擎。store 须为 SnapshotStore。"""
        tip = store.validate_chain()
        engine = cls(match)
        engine.state = BattleState.from_dict({
            "units": tip["state"]["units"],
            "players": [p["id"] for p in tip["state"]["players"]],
        })
        engine.state.quota_used_total = {
            p: int(v) for p, v in tip["quota_used_total"].items()}
        for player, cmds in tip["buffer"].items():
            for raw in cmds:
                cmd = Command.from_dict(raw)
                engine.buffers[player][cmd.client_seq] = cmd
        engine.settled = {p: set(v) for p, v in tip["settled"].items()}
        engine.step_no = int(tip["step"]) + 1
        engine.chain_tip = tip["chain_hash"]
        engine.hash_chain = [
            {"state_hash": store.load(s)["state_hash"],
             "chain_hash": store.load(s)["chain_hash"]}
            for s in range(engine.step_no)]
        while (engine._inbox_pos < len(engine.commands)
               and engine.commands[engine._inbox_pos].recv_tick
               <= tip["step"]):
            engine._inbox_pos += 1
        return engine

    # ---- 基线 / 轨迹导出 -------------------------------------------------

    def trace_lines(self) -> list[dict]:
        return [{
            "frame": r["frame"],
            "applied": r["applied"],
            "buffer_watermark": r["buffer_watermark"],
            "chain_hash": r["chain_hash"],
            "events": r["events"],
            "quota_used": r["quota_used"],
            "state_hash": r["state_hash"],
        } for r in self.records]

    def baseline(self) -> dict:
        return {
            "baseline_version": BASELINE_VERSION,
            "state_version": STATE_VERSION,
            "match_id": self.match_id,
            "frames": self.trace_lines(),
        }
