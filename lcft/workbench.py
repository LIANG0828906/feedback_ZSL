"""Deterministic workbench session and a dependency-free local web UI."""

from __future__ import annotations

import json
import threading
from copy import deepcopy
from dataclasses import replace
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from .budget import BudgetController, select_degradation
from .canonical import atomic_write_json
from .engine import incremental_layout, layout_track
from .errors import LCFTError
from .loader import LoadedBundle, load_bundle
from .params import LayoutParams
from .precompute import GLOBAL_TASK_REGISTRY, TaskRegistry, precompute_track
from .snapshot import SnapshotStore
from .timebase import NormalizedSubtitle
from .version_store import VersionStore
from .wav import iter_channel_peaks


class WorkbenchSession:
    def __init__(self, bundle: LoadedBundle | str | Path, out_dir: str | Path) -> None:
        self.loaded = bundle if isinstance(bundle, LoadedBundle) else load_bundle(bundle)
        self.out_dir = Path(out_dir)
        self.out_dir.mkdir(parents=True, exist_ok=True)
        self.params = LayoutParams()
        self.param_version = 1
        self.tracks: dict[str, tuple[NormalizedSubtitle, ...]] = {
            language: self.loaded.normalized.tracks[language]
            for language in self.loaded.languages
        }
        self.descriptions = self.loaded.normalized.descriptions
        self.reports: dict[str, dict[str, Any]] = {}
        self.stores: dict[str, VersionStore] = {}
        self.refresh = 0
        self.trace: list[dict[str, Any]] = []
        self.budget_usage: dict[str, Any] = {}
        self.budget_ledger: list[dict[str, Any]] = []
        self._waveform: list[dict[str, int]] | None = None
        self._lock = threading.RLock()
        self._trace_lock = threading.RLock()
        self._cancel_event = threading.Event()
        self._thread: threading.Thread | None = None
        self._task_registry = GLOBAL_TASK_REGISTRY
        self.last_error: dict[str, Any] | None = None

    def _emit(self, event: str, **values: Any) -> dict[str, Any]:
        with self._trace_lock:
            self.refresh += 1
            row = {"event": event, "refresh": self.refresh, **values}
            self.trace.append(row)
            return row

    @property
    def description_track(self) -> str:
        return self.loaded.languages[0] if self.loaded.languages else ""

    def waveform(self) -> list[dict[str, int]]:
        if self._waveform is None:
            self._waveform = iter_channel_peaks(
                self.loaded.normalized.audio.path, buckets=768
            )
        return deepcopy(self._waveform)

    def cancel(self) -> None:
        self._cancel_event.set()
        self._emit("cancel_requested", cooperative=True, publishes_partial=False)

    def start_async(
        self,
        operations: list[dict[str, Any]] | None = None,
        *,
        workers: int = 2,
        job_quota: int = 64 * 1024 * 1024,
        global_quota: int = 128 * 1024 * 1024,
    ) -> dict[str, Any]:
        if workers < 1:
            raise ValueError("workers must be at least 1")
        with self._lock:
            if self._thread is not None and self._thread.is_alive():
                raise LCFTError(
                    "E_BUSY",
                    "workbench layout is already running",
                    field="workbench",
                )
            self._cancel_event.clear()
            thread = threading.Thread(
                target=self._async_target,
                args=(deepcopy(operations or []), workers, job_quota, global_quota),
                name="lcft-workbench",
                daemon=True,
            )
            self._thread = thread
            thread.start()
            return {
                "started": True,
                "refresh": self.refresh,
                "thread": thread.name,
            }

    def _async_target(
        self,
        operations: list[dict[str, Any]],
        workers: int,
        job_quota: int,
        global_quota: int,
    ) -> None:
        try:
            self.run_operations(
                operations,
                workers=workers,
                job_quota=job_quota,
                global_quota=global_quota,
            )
        except LCFTError:
            pass

    def join(self, timeout: float | None = None) -> None:
        if self._thread is not None:
            self._thread.join(timeout)

    def _apply_operations(self, operations: list[dict[str, Any]]) -> None:
        for index, operation in enumerate(operations):
            if not isinstance(operation, dict):
                raise LCFTError(
                    "E_INPUT_INVALID",
                    "workbench operation must be an object",
                    field=f"operations[{index}]",
                )
            kind = operation.get("type")
            if kind == "set_params":
                changes = operation.get("changes", {})
                if not isinstance(changes, dict):
                    raise LCFTError(
                        "E_INPUT_INVALID",
                        "set_params.changes must be an object",
                        field=f"operations[{index}].changes",
                    )
                merged = self.params.to_dict()
                merged.update(changes)
                self.params = LayoutParams.from_dict(merged)
                self.params.validate()
                self.param_version += 1
                self._emit(
                    "param_edit",
                    operation=index,
                    changes=changes,
                    param_version=self.param_version,
                    params_hash=self.params.hash,
                )
            elif kind == "edit_sentence":
                language = operation.get("language")
                identifier = operation.get("id")
                if language not in self.tracks:
                    raise LCFTError(
                        "E_INPUT_INVALID",
                        "edit_sentence language is unknown",
                        field=f"operations[{index}].language",
                        details={"language": language},
                    )
                items = list(self.tracks[language])
                for item_index, item in enumerate(items):
                    if item.id == identifier:
                        changes = {
                            key: operation[key]
                            for key in ("text", "speaker", "max_read_speed", "priority")
                            if key in operation
                        }
                        items[item_index] = replace(item, **changes)
                        break
                else:
                    raise LCFTError(
                        "E_INPUT_INVALID",
                        "edit_sentence id is unknown",
                        field=f"operations[{index}].id",
                        details={"id": identifier},
                    )
                self.tracks[language] = tuple(items)
                self._emit(
                    "sentence_edit",
                    operation=index,
                    language=language,
                    sentence_id=identifier,
                    fields=sorted(
                        key
                        for key in ("text", "speaker", "max_read_speed", "priority")
                        if key in operation
                    ),
                )
            elif kind == "edit_description":
                identifier = operation.get("id")
                items = list(self.descriptions)
                for item_index, item in enumerate(items):
                    if item.id == identifier:
                        changes = {
                            key: operation[key]
                            for key in ("text", "max_read_speed", "priority")
                            if key in operation
                        }
                        items[item_index] = replace(item, **changes)
                        self._emit(
                            "description_edit",
                            operation=index,
                            description_id=identifier,
                            fields=sorted(changes),
                        )
                        break
                else:
                    raise LCFTError(
                        "E_INPUT_INVALID",
                        "edit_description id is unknown",
                        field=f"operations[{index}].id",
                        details={"id": identifier},
                    )
                self.descriptions = tuple(items)
            else:
                raise LCFTError(
                    "E_INPUT_INVALID",
                    "unknown workbench operation type",
                    field=f"operations[{index}].type",
                    details={"type": kind},
                )

    def run_operations(
        self,
        operations: list[dict[str, Any]] | None = None,
        *,
        workers: int = 2,
        job_quota: int = 64 * 1024 * 1024,
        global_quota: int = 128 * 1024 * 1024,
    ) -> dict[str, Any]:
        if workers < 1:
            raise ValueError("workers must be at least 1")
        operations = operations or []
        with self._lock:
            self._emit(
                "run_requested",
                operations=len(operations),
                workers=workers,
                param_version=self.param_version,
            )
            self._apply_operations(operations)
            budget = BudgetController(global_quota)
            self.last_error = None
            published: list[str] = []
            for language in self.loaded.languages:
                if self._cancel_event.is_set():
                    self._emit(
                        "cancelled",
                        language=language,
                        cooperative=True,
                        published=False,
                    )
                    return {"ok": False, "cancelled": True, "language": language}
                track_id = f"speech-{language}"
                job = f"workbench-{language}"
                speech = self.tracks[language]
                host_descriptions = (
                    self.descriptions if language == self.description_track else tuple()
                )
                text_bytes = sum(
                    len(item.text.encode("utf-8"))
                    for item in tuple(speech) + tuple(host_descriptions)
                )
                try:
                    usage = select_degradation(
                        sentence_count=len(speech) + len(host_descriptions),
                        text_bytes=text_bytes,
                        job_quota=job_quota,
                        global_quota=global_quota,
                    )
                except LCFTError as exc:
                    self.last_error = exc.to_dict()
                    self._emit(
                        "budget_rejected",
                        language=language,
                        job=job,
                        code=exc.code,
                        field=exc.field,
                        attempts=exc.details.get("attempts", []),
                        published=False,
                    )
                    return {"ok": False, "error": exc.to_dict()}
                self.budget_usage[language] = usage.to_dict()
                try:
                    self._task_registry.acquire(track_id, job)
                except LCFTError as exc:
                    self._emit(
                        "busy",
                        language=language,
                        job=job,
                        code=exc.code,
                    )
                    raise
                try:
                    budget.acquire(job, usage)
                    self._emit(
                        "admitted",
                        language=language,
                        job=job,
                        stage=usage.stage,
                        usage_bytes=usage.total_bytes,
                    )
                    precomputed = precompute_track(
                        track_id,
                        speech,
                        host_descriptions,
                        self.params,
                        self.loaded.normalized.audio.sample_rate,
                        workers=workers,
                        cancel_event=self._cancel_event,
                    )
                    if self._cancel_event.is_set():
                        self._emit(
                            "cancelled",
                            language=language,
                            job=job,
                            cooperative=True,
                            published=False,
                        )
                        return {"ok": False, "cancelled": True, "language": language}
                    previous = self.reports.get(language)
                    next_param_version = self.param_version
                    if previous is None:
                        report = layout_track(
                            track_id,
                            language,
                            speech,
                            host_descriptions,
                            self.loaded.normalized,
                            self.params,
                            published_version=1,
                            param_version=next_param_version,
                            precomputed_sentences=precomputed.sentences,
                            precomputed_descriptions=precomputed.descriptions,
                        )
                        stats = {
                            "affected_count": len(report["sentences"])
                            + len(report["descriptions"]),
                            "reused_count": 0,
                            "affected_ids": [
                                row["id"]
                                for row in report["sentences"] + report["descriptions"]
                            ],
                            "conservative": False,
                            "reasons": ["initial workbench layout"],
                        }
                    else:
                        report, stats = incremental_layout(
                            previous,
                            speech,
                            host_descriptions,
                            self.loaded.normalized,
                            self.params,
                            param_version=next_param_version,
                            published_version=int(previous["published_version"]) + 1,
                        )
                    snapshot = SnapshotStore(
                        self.out_dir / "snapshots" / language, track_id
                    )
                    store = self.stores.get(language)
                    if store is None:
                        store = VersionStore(snapshot_store=snapshot)
                        self.stores[language] = store
                    store.publish(report)
                    self.reports[language] = store.preview() or report
                    affected_ids = list(stats.get("affected_ids", []))
                    by_id = {
                        row["id"]: row
                        for row in report["sentences"] + report["descriptions"]
                    }
                    for identifier in affected_ids:
                        row = by_id.get(identifier)
                        if row is None:
                            continue
                        self._emit(
                            "sentence_conclusion",
                            language=language,
                            sentence_id=identifier,
                            start_timecode=row.get("start_timecode"),
                            end_timecode=row.get("end_timecode"),
                            content_hash=row.get("content_hash"),
                            verdict=row.get("verdict"),
                        )
                    self._emit(
                        "incremental_published" if previous is not None else "published",
                        language=language,
                        track=track_id,
                        version=report["published_version"],
                        track_hash=report["track_hash"],
                        affected=stats["affected_count"],
                        reused=stats["reused_count"],
                        affected_ids=affected_ids,
                        conservative=stats["conservative"],
                        reasons=stats["reasons"],
                        degradation_stage=usage.stage,
                        budget_used_bytes=usage.total_bytes,
                        timecodes={
                            "first": report["sentences"][0]["start_timecode"]
                            if report["sentences"]
                            else None,
                            "last": report["sentences"][-1]["end_timecode"]
                            if report["sentences"]
                            else None,
                        },
                    )
                    published.append(language)
                except LCFTError as exc:
                    self.last_error = exc.to_dict()
                    self._emit(
                        "failed",
                        language=language,
                        job=job,
                        code=exc.code,
                        field=exc.field,
                        published=False,
                    )
                    if exc.code == "E_CANCELLED":
                        return {"ok": False, "cancelled": True, "error": exc.to_dict()}
                    return {"ok": False, "error": exc.to_dict()}
                finally:
                    budget.release(job, usage)
                    self._task_registry.release(track_id, job)
                    self.budget_ledger = deepcopy(budget.ledger)
            self._emit(
                "run_finished",
                published=published,
                ok=True,
            )
            return {"ok": True, "published": published, "refresh": self.refresh}

    def state(self) -> dict[str, Any]:
        with self._lock:
            timelines: dict[str, Any] = {}
            for language in self.loaded.languages:
                report = self.reports.get(language)
                if report is None:
                    timelines[language] = {
                        "sentences": [],
                        "descriptions": [],
                        "contention_edges": [],
                    }
                    continue
                timelines[language] = {
                    "track_hash": report["track_hash"],
                    "published_version": report["published_version"],
                    "sentences": report["sentences"],
                    "descriptions": report["descriptions"],
                    "speaker_marks": report["speaker_marks"],
                    "trims": report["trims"],
                    "contention_edges": report["contention_edges"],
                    "timebase": report["timebase"],
                }
            running = self._thread is not None and self._thread.is_alive()
            return {
                "schema_version": 1,
                "refresh": self.refresh,
                "running": running,
                "seed": self.loaded.raw.get("seed"),
                "shape": self.loaded.raw.get("shape"),
                "languages": self.loaded.languages,
                "description_track": self.description_track,
                "params": self.params.to_dict(),
                "params_hash": self.params.hash,
                "param_version": self.param_version,
                "budget_usage": deepcopy(self.budget_usage),
                "budget_ledger": deepcopy(self.budget_ledger),
                "timelines": timelines,
                "trace": self._trace_snapshot(),
                "last_error": deepcopy(self.last_error),
            }

    def export_trace(self, path: str | Path) -> None:
        atomic_write_json(path, self._trace_snapshot())

    def _trace_snapshot(self) -> list[dict[str, Any]]:
        with self._trace_lock:
            return deepcopy(self.trace)


HTML = r"""<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>LCFT 本地化排版工作台</title>
<style>
:root{--bg:#10141b;--panel:#171d27;--line:#2b3544;--text:#e7edf5;--muted:#8fa1b5;--speech:#52b7ff;--desc:#ffb94a;--fight:#ff5f7a;--ok:#5bd39a}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.45 ui-monospace,SFMono-Regular,Consolas,monospace}
header{padding:16px 20px;border-bottom:1px solid var(--line);display:flex;align-items:center;justify-content:space-between}
h1{font-size:16px;margin:0;letter-spacing:.04em}.muted{color:var(--muted)}.tag{border:1px solid var(--line);padding:3px 8px;border-radius:99px;margin-left:6px}
main{display:grid;grid-template-columns:360px 1fr;min-height:calc(100vh - 62px)}aside{padding:16px;border-right:1px solid var(--line)}
section{padding:16px;overflow:auto}textarea{width:100%;min-height:190px;background:#0c1118;color:var(--text);border:1px solid var(--line);border-radius:8px;padding:10px}
button{background:var(--speech);color:#061018;border:0;border-radius:7px;padding:8px 12px;font-weight:700;cursor:pointer;margin:6px 6px 6px 0}button.secondary{background:var(--line);color:var(--text)}
canvas{width:100%;height:150px;background:#0b1016;border:1px solid var(--line);border-radius:8px}
.timeline{position:relative;height:180px;background:#0b1016;border:1px solid var(--line);border-radius:8px;margin:12px 0;overflow:hidden}
.bar{position:absolute;height:18px;border-radius:3px;opacity:.9;min-width:1px}.speech{background:var(--speech);top:24px}.desc{background:var(--desc);top:56px}
.fight{position:absolute;top:0;bottom:0;background:rgba(255,95,122,.18);border-left:1px solid var(--fight);border-right:1px solid var(--fight)}
.mark{position:absolute;top:88px;width:1px;height:30px;background:var(--ok)}.label{font-size:11px;color:var(--muted);position:absolute;left:8px}
table{width:100%;border-collapse:collapse}td,th{text-align:left;border-bottom:1px solid var(--line);padding:5px 6px}th{color:var(--muted);font-weight:500}
#log{white-space:pre-wrap;max-height:280px;overflow:auto;background:#0c1118;border:1px solid var(--line);border-radius:8px;padding:10px;font-size:12px}
.err{color:var(--fight)}.ok{color:var(--ok)}@media(max-width:900px){main{grid-template-columns:1fr}aside{border-right:0;border-bottom:1px solid var(--line)}}
</style>
</head>
<body>
<header><h1>LCFT 本地化排版工作台</h1><div><span id="status" class="tag">idle</span><span id="refresh" class="tag">refresh 0</span></div></header>
<main>
<aside>
  <div class="muted">参数与操作（JSON）</div>
  <textarea id="ops">[{"type":"set_params","changes":{"min_gap_seconds":"0.08"}}]</textarea>
  <div>
    <label class="muted">并行度 <input id="workers" type="number" min="1" value="2" style="width:60px;background:#0c1118;color:var(--text);border:1px solid var(--line)"></label>
  </div>
  <button onclick="run()">启动 / 增量排版</button>
  <button class="secondary" onclick="cancel()">协作式取消</button>
  <button class="secondary" onclick="loadState()">刷新</button>
  <div id="budget" class="muted" style="margin-top:14px"></div>
  <div id="params" class="muted" style="margin-top:8px"></div>
  <div id="error" class="err" style="margin-top:10px"></div>
</aside>
<section>
  <div class="muted">波形（首声道峰值）</div>
  <canvas id="wave" width="1200" height="150"></canvas>
  <div id="timelines"></div>
  <div class="muted">轨迹日志</div>
  <div id="log"></div>
</section>
</main>
<script>
let current=null;
async function getJson(url,opt){const r=await fetch(url,opt);return await r.json()}
async function loadState(){
  current=await getJson('/api/state');
  document.getElementById('refresh').textContent='refresh '+current.refresh;
  document.getElementById('params').textContent=JSON.stringify(current.params);
  document.getElementById('status').textContent=current.running?'running':'idle';
  document.getElementById('error').textContent=current.last_error?JSON.stringify(current.last_error):'';
  document.getElementById('budget').textContent='预算：'+JSON.stringify(current.budget_usage);
  document.getElementById('log').textContent=JSON.stringify(current.trace.slice(-40),null,2);
  renderTimelines(current.timelines);
  const w=await getJson('/api/waveform'); drawWave(w.peaks);
}
function drawWave(peaks){
  const c=document.getElementById('wave'),ctx=c.getContext('2d'),W=c.width,H=c.height;
  ctx.clearRect(0,0,W,H);ctx.strokeStyle='#52b7ff';ctx.beginPath();
  peaks.forEach((p,i)=>{const x=i*W/Math.max(1,peaks.length-1),a=(p.peak/32768)*(H*.42);ctx.moveTo(x,H/2-a);ctx.lineTo(x,H/2+a)});
  ctx.stroke();ctx.strokeStyle='#2b3544';ctx.beginPath();ctx.moveTo(0,H/2);ctx.lineTo(W,H/2);ctx.stroke();
}
function renderTimelines(tl){
  const root=document.getElementById('timelines');root.innerHTML='';
  for(const [lang,t] of Object.entries(tl)){
    const all=[...(t.sentences||[]),...(t.descriptions||[])].filter(x=>x.start_ticks!==null&&x.start_ticks!==undefined);
    const max=Math.max(1,...all.map(x=>x.end_ticks));
    const box=document.createElement('div');box.innerHTML='<div class="muted">'+lang+'  '+(t.track_hash||'')+'</div>';
    const line=document.createElement('div');line.className='timeline';
    line.innerHTML='<div class="label">voice</div><div class="label" style="top:32px">desc</div><div class="label" style="top:64px">speaker</div>';
    (t.sentences||[]).forEach(s=>{const b=document.createElement('div');b.className='bar speech';b.style.left=(100*s.start_ticks/max)+'%';b.style.width=(100*(s.end_ticks-s.start_ticks)/max)+'%';b.title=s.id+' '+s.verdict;line.appendChild(b)});
    (t.descriptions||[]).forEach(s=>{if(s.start_ticks===null)return;const b=document.createElement('div');b.className='bar desc';b.style.left=(100*s.start_ticks/max)+'%';b.style.width=(100*(s.end_ticks-s.start_ticks)/max)+'%';b.title=s.id+' priority '+s.priority;line.appendChild(b)});
    (t.speaker_marks||[]).forEach(m=>{const b=document.createElement('div');b.className='mark';b.style.left=(100*m.at_ticks/max)+'%';line.appendChild(b)});
    (t.contention_edges||[]).forEach((e,i)=>{const s=(t.sentences||[]).find(x=>x.id===e.from)||(t.descriptions||[]).find(x=>x.id===e.from);if(!s||s.start_ticks==null)return;const b=document.createElement('div');b.className='fight';b.style.left=(100*s.start_ticks/max)+'%';b.style.width=Math.max(.3,100*(e.contention_ticks||0)/max)+'%';b.title=e.from+' → '+e.to;line.appendChild(b)});
    box.appendChild(line);root.appendChild(box);
  }
}
async function run(){
  let ops;try{ops=JSON.parse(document.getElementById('ops').value)}catch(e){alert('JSON 错误: '+e);return}
  const workers=Number(document.getElementById('workers').value||2);
  const r=await getJson('/api/run',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({operations:ops,workers})});
  if(!r.started)alert(JSON.stringify(r));
  setTimeout(loadState,300);
}
async function cancel(){await getJson('/api/cancel',{method:'POST'});loadState()}
loadState();setInterval(loadState,1500);
</script>
</body></html>"""


def make_handler(session: WorkbenchSession) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        server_version = "LCFTWorkbench/0.1"

        def log_message(self, format: str, *args: Any) -> None:
            return

        def _send_json(self, value: Any, status: int = 200) -> None:
            data = json.dumps(
                value, ensure_ascii=False, sort_keys=True, separators=(",", ":")
            ).encode("utf-8")
            self.send_response(status)
            self.send_header("content-type", "application/json; charset=utf-8")
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def _send_html(self) -> None:
            data = HTML.encode("utf-8")
            self.send_response(HTTPStatus.OK)
            self.send_header("content-type", "text/html; charset=utf-8")
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self) -> None:
            path = urlparse(self.path).path
            if path == "/":
                self._send_html()
            elif path == "/api/state":
                self._send_json(session.state())
            elif path == "/api/waveform":
                self._send_json({"peaks": session.waveform()})
            elif path == "/api/trace":
                self._send_json({"refresh": session.refresh, "trace": session.trace})
            else:
                self._send_json({"code": "E_NOT_FOUND"}, 404)

        def do_POST(self) -> None:
            path = urlparse(self.path).path
            length = int(self.headers.get("content-length", "0"))
            raw = self.rfile.read(length) if length else b"{}"
            try:
                body = json.loads(raw.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError):
                self._send_json(
                    {
                        "code": "E_INPUT_INVALID",
                        "field": "request.body",
                        "message": "invalid JSON",
                    },
                    HTTPStatus.BAD_REQUEST,
                )
                return
            try:
                if path == "/api/run":
                    result = session.start_async(
                        body.get("operations", []),
                        workers=int(body.get("workers", 2)),
                        job_quota=int(body.get("job_quota", 64 * 1024 * 1024)),
                        global_quota=int(body.get("global_quota", 128 * 1024 * 1024)),
                    )
                    self._send_json(result, HTTPStatus.ACCEPTED)
                elif path == "/api/cancel":
                    session.cancel()
                    self._send_json({"cancelled": True, "refresh": session.refresh})
                else:
                    self._send_json({"code": "E_NOT_FOUND"}, 404)
            except LCFTError as exc:
                self._send_json(exc.to_dict(), HTTPStatus.CONFLICT)
            except (TypeError, ValueError) as exc:
                self._send_json(
                    {
                        "code": "E_INPUT_INVALID",
                        "field": "request.body",
                        "message": str(exc),
                    },
                    HTTPStatus.BAD_REQUEST,
                )

    return Handler


def serve_workbench(
    bundle: LoadedBundle | str | Path,
    out_dir: str | Path,
    *,
    host: str = "127.0.0.1",
    port: int = 8765,
) -> None:
    session = WorkbenchSession(bundle, out_dir)
    server = ThreadingHTTPServer((host, port), make_handler(session))
    print(f"LCFT workbench: http://{host}:{server.server_port}/")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()

