"""Local browser diagnostic dashboard and self-contained export."""

from __future__ import annotations

import html
import json
import math
import threading
from dataclasses import dataclass
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

import numpy as np

from .comparison import compare_results, diff_heatmap
from .engine import RenderEngine, RenderOutcome
from .errors import PipelineError, parameter_error
from .renderer import RenderConfig, block_windows
from .resources import ResourceLimits
from .scene import Scene
from .util import normalize, write_json_atomic


_DASHBOARD_HTML = r"""<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>透明介质与体积光诊断面板</title>
<style>
:root { color-scheme: dark; --bg:#0c1119; --panel:#131c28; --line:#263448; --ink:#e8f1ff; --muted:#8ea3bf; --accent:#58c7ff; --ok:#4be3a5; --bad:#ff6b7c; --warn:#ffd166; --mid:#8b9dff; }
* { box-sizing: border-box; }
body { margin:0; background:radial-gradient(circle at 25% -20%,#17314c 0,#0c1119 45%); color:var(--ink); font:14px/1.5 ui-sans-serif,system-ui,"Segoe UI",sans-serif; }
header { padding:18px 22px 10px; display:flex; align-items:end; justify-content:space-between; gap:12px; flex-wrap:wrap; }
h1 { margin:0; font-size:20px; letter-spacing:.02em; }
.sub { color:var(--muted); font-size:12px; }
.wrap { padding:0 22px 28px; display:grid; gap:14px; grid-template-columns:minmax(0,1.5fr) minmax(320px,.8fr); }
.panel { background:linear-gradient(180deg,rgba(25,38,55,.96),rgba(17,26,39,.96)); border:1px solid var(--line); border-radius:14px; box-shadow:0 12px 35px #0006; overflow:hidden; }
.panel h2 { margin:0; font-size:13px; font-weight:650; letter-spacing:.04em; color:#b8cbe4; padding:10px 14px; border-bottom:1px solid var(--line); text-transform:uppercase; }
.panel .body { padding:12px 14px; }
.canvas-stack { position:relative; width:100%; aspect-ratio:4/3; background:#070b11; border-radius:8px; overflow:hidden; }
.canvas-stack canvas { position:absolute; inset:0; width:100%; height:100%; image-rendering:pixelated; }
.controls { display:grid; grid-template-columns:1fr 1fr; gap:10px; }
.control { padding:9px; border:1px solid var(--line); border-radius:9px; background:#0e1722; }
.control label { display:block; color:var(--muted); font-size:11px; margin-bottom:4px; }
select,input { width:100%; background:#0a121c; color:var(--ink); border:1px solid #30435e; border-radius:6px; padding:6px; }
button { background:#163b58; color:#d8f3ff; border:1px solid #2e6e96; border-radius:7px; padding:7px 10px; cursor:pointer; font-weight:600; }
button:hover { background:#1b4c70; }
button.primary { background:#0e6b58; border-color:#21a889; }
.row { display:flex; gap:8px; align-items:center; flex-wrap:wrap; margin-top:8px; }
.pill { padding:3px 8px; border-radius:999px; background:#17283b; color:#b6cce7; font-size:12px; }
.pill.ok { background:#0c3b2f; color:var(--ok); } .pill.bad { background:#431b23; color:var(--bad); }
table { width:100%; border-collapse:collapse; font-size:12px; }
th,td { text-align:left; padding:5px 6px; border-bottom:1px solid #1f2b3c; }
th { color:var(--muted); font-weight:600; }
.legend { display:flex; gap:12px; flex-wrap:wrap; color:var(--muted); font-size:11px; margin-top:6px; }
.dot { width:8px; height:8px; border-radius:50%; display:inline-block; margin-right:4px; }
.statusgrid { display:grid; grid-template-columns:repeat(3,1fr); gap:4px; }
.block { min-height:52px; border-radius:6px; border:1px solid #ffffff22; display:flex; align-items:center; justify-content:center; font-size:11px; text-align:center; padding:4px; }
.error { color:var(--bad); white-space:pre-wrap; }
.muted { color:var(--muted); }
.wide { grid-column:1/-1; }
#log { font-family:ui-monospace,SFMono-Regular,Consolas,monospace; font-size:11px; white-space:pre-wrap; color:#a9c2df; max-height:130px; overflow:auto; }
@media (max-width:900px) { .wrap { grid-template-columns:1fr; } .controls { grid-template-columns:1fr; } }
</style>
</head>
<body>
<header>
  <div><h1>透明介质与体积光 · 诊断面板</h1><div class="sub" id="subtitle">本地确定性软件光栅</div></div>
  <div class="row" id="summary"></div>
</header>
<main class="wrap">
  <section class="panel">
    <h2>介质边界与透射率热力图</h2>
    <div class="body">
      <div class="canvas-stack"><canvas id="transCanvas" width="48" height="36"></canvas><canvas id="boundaryCanvas" width="480" height="360"></canvas></div>
      <div class="legend"><span><i class="dot" style="background:#58c7ff"></i>玻璃球</span><span><i class="dot" style="background:#8b9dff"></i>雾腔</span><span><i class="dot" style="background:#4be3a5"></i>水面层</span><span>亮度 = 透射率</span></div>
      <div class="row"><span class="pill" id="progressPill">-</span><span class="pill" id="stepPill">-</span><span class="pill" id="resumePill">-</span></div>
    </div>
  </section>
  <section class="panel">
    <h2>参数编辑与增量重算</h2>
    <div class="body controls">
      <div class="control">
        <label>介质</label><select id="mediumSelect"></select>
        <label style="margin-top:6px">吸收倍率 <input id="absorptionScale" type="number" value="1.05" step="0.05" min="0"></label>
        <label>散射倍率 <input id="scatteringScale" type="number" value="1.00" step="0.05" min="0"></label>
        <button style="margin-top:7px" onclick="editMedium()">应用并增量重算</button>
      </div>
      <div class="control">
        <label>体积光源</label><select id="lightSelect"></select>
        <label style="margin-top:6px">逐波长强度倍率 <input id="lightScale" type="number" value="1.05" step="0.05" min="0"></label>
        <label>离散步长 <input id="stepSize" type="number" value="0.05" step="0.005" min="0.001"></label>
        <button style="margin-top:7px" onclick="editLight()">应用光源并增量重算</button>
        <button style="margin-top:7px" onclick="changeStep()">按步长全量重算</button>
      </div>
      <div class="row wide">
        <button class="primary" onclick="runCompare()">与全量重算逐像素比对</button>
        <button onclick="retryFailed()">重试失败块</button>
        <button onclick="exportAll()">导出结果与面板</button>
      </div>
      <div id="log" class="wide muted">就绪。</div>
    </div>
  </section>
  <section class="panel">
    <h2>分块状态矩阵</h2>
    <div class="body"><div class="statusgrid" id="statusGrid"></div></div>
  </section>
  <section class="panel">
    <h2>失效传播清单</h2>
    <div class="body"><table id="invalidationTable"><thead><tr><th>块</th><th>资源</th><th>阶段</th><th>原因</th></tr></thead><tbody></tbody></table></div>
  </section>
  <section class="panel wide">
    <h2>差异热力图</h2>
    <div class="body"><div class="canvas-stack" style="aspect-ratio:4/3;max-width:620px"><canvas id="diffCanvas" width="48" height="36"></canvas></div><div id="diffMeta" class="muted">尚未运行全量比对。</div></div>
  </section>
</main>
<script>
let state = null;
const statusColors = {pending_intersect:"#39465d",transmission_accumulating:"#2d6da3",scattering_accumulating:"#7760bd",committed:"#1f8b67",failed:"#b43c4b"};
const mediumColors = ["#58c7ff","#8b9dff","#4be3a5","#ffd166"];
function $(id){ return document.getElementById(id); }
function log(msg){ $("log").textContent = msg; }
async function api(url, options={}){
  const response = await fetch(url, options);
  const value = await response.json();
  if(!value.ok && value.error){ throw new Error(value.error.code + " [" + value.error.field + "] " + value.error.message); }
  return value;
}
function drawHeatmap(canvas, heat){
  if(!heat || !heat.length) return;
  const h=heat.length, w=heat[0].length;
  const off=document.createElement("canvas"); off.width=w; off.height=h;
  const ctx=off.getContext("2d"); const img=ctx.createImageData(w,h);
  for(let y=0;y<h;y++) for(let x=0;x<w;x++){
    const p=heat[y][x], i=(y*w+x)*4;
    img.data[i]=p[0]; img.data[i+1]=p[1]; img.data[i+2]=p[2]; img.data[i+3]=255;
  }
  ctx.putImageData(img,0,0);
  const out=canvas.getContext("2d"); out.imageSmoothingEnabled=false;
  out.clearRect(0,0,canvas.width,canvas.height); out.drawImage(off,0,0,canvas.width,canvas.height);
}
function drawBoundaries(boundaries){
  const canvas=$("boundaryCanvas"), ctx=canvas.getContext("2d");
  ctx.clearRect(0,0,canvas.width,canvas.height);
  const sx=canvas.width/(state.width||1), sy=canvas.height/(state.height||1);
  boundaries.forEach((poly, idx)=>{
    ctx.lineWidth=1.4; ctx.strokeStyle=poly.color || mediumColors[idx%mediumColors.length];
    (poly.polylines || []).forEach(points=>{
      if(!points.length) return;
      ctx.beginPath();
      points.forEach((p,i)=>{ const x=p[0]*sx, y=p[1]*sy; if(i===0) ctx.moveTo(x,y); else ctx.lineTo(x,y); });
      ctx.stroke();
    });
  });
}
function drawDiff(heat){
  if(!heat){ $("diffCanvas").getContext("2d").clearRect(0,0,48,36); return; }
  const h=heat.length,w=heat[0].length, off=document.createElement("canvas"); off.width=w; off.height=h;
  const ctx=off.getContext("2d"), img=ctx.createImageData(w,h);
  for(let y=0;y<h;y++)for(let x=0;x<w;x++){ const v=Math.min(255,Math.round((heat[y][x]||0)*255)),i=(y*w+x)*4; img.data[i]=255;img.data[i+1]=90;img.data[i+2]=v>0?110:90;img.data[i+3]=255; }
  ctx.putImageData(img,0,0); const out=$("diffCanvas").getContext("2d"); out.imageSmoothingEnabled=false; out.clearRect(0,0,48,36); out.drawImage(off,0,0,48,36);
}
function renderState(next){
  state=next;
  $("subtitle").textContent = "场景 " + next.scene_hash.slice(0,12) + " · 算法 " + next.algorithm_version + " · " + next.width + "×" + next.height;
  const p=next.progress||{}, ok=next.complete && !Object.keys(next.failed_blocks||{}).length;
  $("summary").innerHTML = `<span class="pill ${ok?"ok":"bad"}">${ok?"完整":"未完成"}</span><span class="pill">${p.committed||0}/${p.total||0} 块</span><span class="pill">${next.mode||""}</span>`;
  $("progressPill").textContent = `已提交 ${p.committed||0}/${p.total||0}`;
  $("stepPill").textContent = `实际步长 ${next.actual_step} · 误差界 ${next.preview_error_bound}`;
  $("resumePill").textContent = `恢复 ${(next.resume&&(next.resume.action||next.resume.action))||"full"}`;
  drawHeatmap($("transCanvas"), next.transmission_heatmap);
  drawBoundaries(next.boundaries);
  const invalid = new Map();
  (next.invalidation_manifest||[]).forEach(e=>invalid.set(Number(e.block_id), e));
  const columns = Math.max(1, ...(next.blocks||[]).map(b=>Number(b.col)+1));
  $("statusGrid").style.gridTemplateColumns = `repeat(${columns},1fr)`;
  $("statusGrid").innerHTML = (next.blocks||[]).map(b=>{
    const hit = invalid.get(Number(b.id));
    const outline = hit ? "box-shadow:0 0 0 2px #ffd166 inset;" : "";
    const title = hit ? `${b.status} · ${hit.resource_kind}:${hit.resource_id} · ${hit.reason}` : b.status;
    return `<div class="block" style="background:${statusColors[b.status]||"#39465d"};${outline}" title="${title}">#${b.id}<br>${b.status}${hit?"<br>失效":""}</div>`;
  }).join("");
  const tbody=$("invalidationTable").querySelector("tbody"); tbody.innerHTML="";
  (next.invalidation_manifest||[]).slice().reverse().forEach(e=>{
    const tr=document.createElement("tr");
    [e.block_id,e.resource_kind+":"+e.resource_id,e.stage,e.reason].forEach(v=>{const td=document.createElement("td");td.textContent=v;tr.appendChild(td);});
    tbody.appendChild(tr);
  });
  if(!tbody.children.length){ const tr=document.createElement("tr"); const td=document.createElement("td");td.colSpan=4;td.textContent="暂无失效（未编辑或全量重算）";tr.appendChild(td);tbody.appendChild(tr); }
  if(next.diff){ drawDiff(next.diff.heatmap); $("diffMeta").textContent = next.diff.equal ? "逐像素一致。" : "首个差异：" + JSON.stringify(next.diff.report.error&&next.diff.report.error.details); }
  else { drawDiff(null); $("diffMeta").textContent="尚未运行全量比对。"; }
  fillSelects(next.scene);
}
function fillSelects(scene){
  if(!scene) return;
  const m=$("mediumSelect"), l=$("lightSelect");
  if(!m.options.length) m.innerHTML=scene.media.map(x=>`<option value="${x.id}">${x.id}</option>`).join("");
  if(!l.options.length) l.innerHTML=scene.lights.map(x=>`<option value="${x.id}">${x.id}</option>`).join("");
}
async function refresh(){ try{ renderState(await api("/api/state")); }catch(e){ log(e.message); } }
async function editMedium(){
  try{
    log("编辑介质并增量重算...");
    const payload={action:"edit_medium",medium_id:$("mediumSelect").value,absorption_scale:Number($("absorptionScale").value),scattering_scale:Number($("scatteringScale").value)};
    const next=await api("/api/render",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(payload)});
    renderState(next); log("增量重算完成：" + (next.progress.committed+"/"+next.progress.total)+" 块提交。");
  }catch(e){ log("失败："+e.message); }
}
async function editLight(){
  try{
    log("编辑体积光源并增量重算...");
    const payload={action:"edit_light",light_id:$("lightSelect").value,intensity_scale:Number($("lightScale").value)};
    const next=await api("/api/render",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(payload)});
    renderState(next); log("增量重算完成：" + (next.progress.committed+"/"+next.progress.total)+" 块提交。");
  }catch(e){ log("失败："+e.message); }
}
async function changeStep(){
  try{
    log("步长变化，执行全量重算...");
    const payload={action:"set_step",step_size:Number($("stepSize").value)};
    const next=await api("/api/render",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(payload)});
    renderState(next); log("全量重算完成，实际步长 "+next.actual_step+"。");
  }catch(e){ log("失败："+e.message); }
}
async function runCompare(){
  try{
    log("执行一次性全量重算并逐像素比对...");
    const next=await api("/api/compare",{method:"POST",headers:{"Content-Type":"application/json"},body:"{}"});
    renderState(next);
    log(next.diff.equal ? "比对通过：透射率、散射、统计量与提交序列一致。" : "比对失败："+JSON.stringify(next.diff.report.error));
  }catch(e){ log("失败："+e.message); }
}
async function retryFailed(){
  try{
    const payload={action:"retry_failed"};
    const next=await api("/api/render",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(payload)});
    renderState(next); log("失败块重试完成：" + (next.progress.committed+"/"+next.progress.total));
  }catch(e){ log("失败："+e.message); }
}
async function exportAll(){
  try{
    log("正在导出...");
    const value=await api("/api/export",{method:"POST",headers:{"Content-Type":"application/json"},body:"{}"});
    log("已导出：\n" + Object.values(value.files).join("\n"));
  }catch(e){ log("失败："+e.message); }
}
refresh();
</script>
</body>
</html>
"""


def _project_point(scene: Scene, sample_index: int, point: np.ndarray) -> tuple[float, float, bool]:
    sample = scene.camera["trajectory"][sample_index]
    eye = np.asarray(sample["eye"], dtype=np.float64)
    target = np.asarray(sample["target"], dtype=np.float64)
    up = np.asarray(sample["up"], dtype=np.float64)
    forward = normalize(target - eye, "camera.forward")
    right = normalize(np.cross(forward, up), "camera.right")
    true_up = normalize(np.cross(right, forward), "camera.up")
    relative = point - eye
    depth = float(np.dot(relative, forward))
    if depth <= float(scene.camera["near_far"][0]):
        return 0.0, 0.0, False
    width = int(scene.camera["width"])
    height = int(scene.camera["height"])
    tangent = math.tan(math.radians(float(scene.camera["fov_y_deg"])) * 0.5)
    aspect = width / height
    x_ndc = float(np.dot(relative, right)) / (depth * tangent * aspect)
    y_ndc = float(np.dot(relative, true_up)) / (depth * tangent)
    x = (x_ndc + 1.0) * width * 0.5 - 0.5
    y = (1.0 - y_ndc) * height * 0.5 - 0.5
    return x, y, True


def project_boundaries(scene: Scene, sample_index: int) -> list[dict[str, Any]]:
    colors = ["#58c7ff", "#8b9dff", "#4be3a5", "#ffd166"]
    result: list[dict[str, Any]] = []
    for medium_index, medium in enumerate(scene.media):
        boundary = medium["boundary"]
        segments: list[tuple[np.ndarray, np.ndarray]] = []
        if boundary["type"] == "box":
            lower = np.asarray(boundary["min"], dtype=np.float64)
            upper = np.asarray(boundary["max"], dtype=np.float64)
            corners = [
                np.array([x, y, z], dtype=np.float64)
                for x in (lower[0], upper[0])
                for y in (lower[1], upper[1])
                for z in (lower[2], upper[2])
            ]
            for a in range(8):
                for b in range(a + 1, 8):
                    if int(np.sum(corners[a] != corners[b])) == 1:
                        segments.append((corners[a], corners[b]))
        elif boundary["type"] == "sphere":
            center = np.asarray(boundary["center"], dtype=np.float64)
            radius = float(boundary["radius"])
            for axis in range(3):
                first = (axis + 1) % 3
                second = (axis + 2) % 3
                ring: list[np.ndarray] = []
                for step in range(49):
                    angle = 2.0 * math.pi * step / 48.0
                    point = center.copy()
                    point[first] += radius * math.cos(angle)
                    point[second] += radius * math.sin(angle)
                    ring.append(point)
                segments.extend((ring[index], ring[index + 1]) for index in range(48))
        elif boundary["type"] == "mesh":
            vertices = [np.asarray(vertex, dtype=np.float64) for vertex in boundary["vertices"]]
            for face in boundary["faces"]:
                segments.extend(
                    [
                        (vertices[face[0]], vertices[face[1]]),
                        (vertices[face[1]], vertices[face[2]]),
                        (vertices[face[2]], vertices[face[0]]),
                    ]
                )
        projected: list[list[list[float]]] = []
        current: list[list[float]] = []
        for start, end in segments:
            x0, y0, visible0 = _project_point(scene, sample_index, start)
            x1, y1, visible1 = _project_point(scene, sample_index, end)
            if not (visible0 and visible1):
                if len(current) > 1:
                    projected.append(current)
                current = []
                continue
            if current and current[-1] == [x0, y0]:
                current.append([x1, y1])
            else:
                if len(current) > 1:
                    projected.append(current)
                current = [[x0, y0], [x1, y1]]
        if len(current) > 1:
            projected.append(current)
        result.append(
            {
                "id": medium["id"],
                "type": boundary["type"],
                "color": colors[medium_index % len(colors)],
                "polylines": projected,
            }
        )
    return result


@dataclass
class Dashboard:
    scene: Scene
    config: RenderConfig
    checkpoint_directory: Path
    export_directory: Path
    sample_index: int = 0
    limits: ResourceLimits | None = None

    def __post_init__(self) -> None:
        self.lock = threading.RLock()
        self.engine = RenderEngine(
            self.scene,
            self.config,
            checkpoint_directory=self.checkpoint_directory,
            limits=self.limits,
            sample_index=self.sample_index,
        )
        self.outcome: RenderOutcome = self.engine.render(mode="full")
        self.last_diff: dict[str, Any] | None = None
        self.last_retried: list[int] = []

    def _state_payload(self) -> dict[str, Any]:
        outcome = self.outcome
        scene = self.engine.scene
        payload = outcome.to_dict(include_arrays=False, include_heatmap=True)
        payload["width"] = int(scene.camera["width"])
        payload["height"] = int(scene.camera["height"])
        payload["scene"] = scene.to_dict()
        payload["boundaries"] = project_boundaries(scene, self.sample_index)
        payload["diff"] = self.last_diff
        payload["last_retried"] = list(self.last_retried)
        return payload

    def state(self) -> dict[str, Any]:
        with self.lock:
            return self._state_payload()

    def render_action(self, payload: dict[str, Any]) -> dict[str, Any]:
        with self.lock:
            action = payload.get("action")
            try:
                if action == "edit_medium":
                    medium_id = str(payload.get("medium_id", ""))
                    scene = self.engine.scene
                    if medium_id not in scene.media_by_id:
                        raise parameter_error("medium_id", "unknown medium")
                    medium = scene.media_by_id[medium_id]
                    absorption_scale = float(payload.get("absorption_scale", 1.0))
                    scattering_scale = float(payload.get("scattering_scale", 1.0))
                    if absorption_scale < 0.0 or scattering_scale < 0.0:
                        raise parameter_error("medium_scale", "scale factors must be non-negative")
                    updates = {
                        "absorption": [value * absorption_scale for value in medium["absorption"]],
                        "scattering": [value * scattering_scale for value in medium["scattering"]],
                    }
                    self.engine.edit_media(medium_id, updates)
                    self.scene = self.engine.scene.copy()
                    self.outcome = self.engine.render(mode="incremental")
                    self.last_diff = None
                    self.last_retried = []
                elif action == "edit_light":
                    light_id = str(payload.get("light_id", ""))
                    scene = self.engine.scene
                    if light_id not in scene.light_by_id:
                        raise parameter_error("light_id", "unknown light")
                    light = scene.light_by_id[light_id]
                    scale = float(payload.get("intensity_scale", 1.0))
                    if scale < 0.0:
                        raise parameter_error("intensity_scale", "scale factor must be non-negative")
                    self.engine.edit_light(
                        light_id,
                        {"radiance": [value * scale for value in light["radiance"]]},
                    )
                    self.scene = self.engine.scene.copy()
                    self.outcome = self.engine.render(mode="incremental")
                    self.last_diff = None
                    self.last_retried = []
                elif action == "set_step":
                    step = float(payload.get("step_size", self.config.step_size))
                    new_config = RenderConfig(
                        block_width=self.config.block_width,
                        block_height=self.config.block_height,
                        step_size=step,
                        sampling_strategy=self.config.sampling_strategy,
                        max_segments=self.config.max_segments,
                        preview_step_size=None,
                    )
                    self.scene = self.engine.scene.copy()
                    self.config = new_config
                    self.engine = RenderEngine(
                        self.scene,
                        self.config,
                        checkpoint_directory=self.checkpoint_directory,
                        limits=self.limits,
                        sample_index=self.sample_index,
                    )
                    self.outcome = self.engine.render(mode="full")
                    self.last_diff = None
                    self.last_retried = []
                elif action == "retry_failed":
                    retried = self.engine.retry_failed()
                    self.outcome = self.engine.render(mode="incremental")
                    self.last_diff = None
                    self.last_retried = retried
                else:
                    raise parameter_error("action", f"unknown dashboard action {action!r}")
                return self._state_payload()
            except PipelineError as exc:
                result = exc.to_dict()
                result["state"] = self._state_payload()
                return result

    def compare_full(self) -> dict[str, Any]:
        with self.lock:
            reference_engine = RenderEngine(
                self.engine.scene.copy(),
                self.config,
                checkpoint_directory=None,
                limits=self.limits,
                sample_index=self.sample_index,
            )
            reference = reference_engine.render(mode="full")
            report = compare_results(self.outcome, reference)
            self.last_diff = {
                "equal": report.equal,
                "report": report.to_dict(),
                "heatmap": diff_heatmap(
                    np.concatenate([self.outcome.transmission, self.outcome.scattering], axis=2),
                    np.concatenate([reference.transmission, reference.scattering], axis=2),
                ),
            }
            payload = self._state_payload()
            payload.update(report.to_dict())
            payload["ok"] = report.equal
            payload["diff"] = self.last_diff
            return payload

    def export_all(self) -> dict[str, Any]:
        with self.lock:
            self.export_directory.mkdir(parents=True, exist_ok=True)
            files = self.outcome.export(self.export_directory, include_arrays=True)
            write_json_atomic(self.export_directory / "scene.json", self.engine.scene.to_dict())
            dashboard_path = self.export_directory / "dashboard.html"
            state_path = self.export_directory / "dashboard_state.json"
            payload = self._state_payload()
            write_json_atomic(state_path, payload)
            dashboard_path.write_text(_dashboard_html_with_state(payload), encoding="utf-8")
            files["dashboard"] = str(dashboard_path)
            files["dashboard_state"] = str(state_path)
            write_json_atomic(self.export_directory / "export_manifest.json", {"files": files})
            return {"ok": True, "files": files}

    def export_json(self) -> dict[str, Any]:
        with self.lock:
            return self._state_payload()


def serve_dashboard(
    scene: Scene,
    config: RenderConfig,
    *,
    checkpoint_directory: str | Path = "runtime/ui-checkpoints",
    export_directory: str | Path = "runtime/exports",
    sample_index: int = 0,
    host: str = "127.0.0.1",
    port: int = 8765,
    limits: ResourceLimits | None = None,
) -> None:
    server, _ = create_dashboard_server(
        scene,
        config,
        checkpoint_directory=checkpoint_directory,
        export_directory=export_directory,
        sample_index=sample_index,
        host=host,
        port=port,
        limits=limits,
    )
    print(f"Diagnostic dashboard: http://{host}:{server.server_address[1]}/")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


def create_dashboard_server(
    scene: Scene,
    config: RenderConfig,
    *,
    checkpoint_directory: str | Path = "runtime/ui-checkpoints",
    export_directory: str | Path = "runtime/exports",
    sample_index: int = 0,
    host: str = "127.0.0.1",
    port: int = 8765,
    limits: ResourceLimits | None = None,
) -> tuple[ThreadingHTTPServer, Dashboard]:
    dashboard = Dashboard(
        scene,
        config,
        Path(checkpoint_directory),
        Path(export_directory),
        sample_index=sample_index,
        limits=limits,
    )

    class Handler(BaseHTTPRequestHandler):
        server_version = "TransparentVolumeUI/1.0"

        def _send_json(self, value: dict[str, Any], status: int = HTTPStatus.OK) -> None:
            encoded = json.dumps(value, ensure_ascii=False).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def _read_json(self) -> dict[str, Any]:
            length = int(self.headers.get("Content-Length", "0"))
            if length <= 0:
                return {}
            return json.loads(self.rfile.read(length).decode("utf-8"))

        def do_GET(self) -> None:  # noqa: N802
            path = urlparse(self.path).path
            if path == "/":
                encoded = _DASHBOARD_HTML.encode("utf-8")
                self.send_response(HTTPStatus.OK)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(encoded)))
                self.end_headers()
                self.wfile.write(encoded)
            elif path == "/api/state":
                self._send_json(dashboard.state())
            elif path == "/api/export.json":
                self._send_json(dashboard.export_json())
            else:
                self._send_json({"ok": False, "error": {"code": "E_SCHEMA", "field": "path", "message": "not found"}}, 404)

        def do_POST(self) -> None:  # noqa: N802
            path = urlparse(self.path).path
            try:
                payload = self._read_json()
                if path == "/api/render":
                    self._send_json(dashboard.render_action(payload))
                elif path == "/api/compare":
                    self._send_json(dashboard.compare_full())
                elif path == "/api/export":
                    self._send_json(dashboard.export_all())
                else:
                    self._send_json(
                        {"ok": False, "error": {"code": "E_SCHEMA", "field": "path", "message": "not found"}},
                        HTTPStatus.NOT_FOUND,
                    )
            except PipelineError as exc:
                self._send_json(exc.to_dict(), HTTPStatus.BAD_REQUEST)
            except Exception as exc:
                self._send_json(
                    {
                        "ok": False,
                        "error": {"code": "E_DECODE", "field": "request", "message": f"{type(exc).__name__}: {exc}"},
                    },
                    HTTPStatus.BAD_REQUEST,
                )

        def log_message(self, format: str, *args: Any) -> None:
            return

    server = ThreadingHTTPServer((host, port), Handler)
    return server, dashboard


def export_static_dashboard(
    engine: RenderEngine,
    outcome: RenderOutcome,
    directory: str | Path,
    *,
    sample_index: int = 0,
) -> dict[str, str]:
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    dashboard_path = directory / "dashboard.html"
    payload = outcome.to_dict(include_arrays=False, include_heatmap=True)
    payload["width"] = int(engine.scene.camera["width"])
    payload["height"] = int(engine.scene.camera["height"])
    payload["scene"] = engine.scene.to_dict()
    payload["boundaries"] = project_boundaries(engine.scene, sample_index)
    payload["diff"] = None
    state_path = directory / "dashboard_state.json"
    write_json_atomic(state_path, payload)
    dashboard_path.write_text(_dashboard_html_with_state(payload), encoding="utf-8")
    return {"dashboard": str(dashboard_path), "state": str(state_path)}


def _dashboard_html_with_state(payload: dict[str, Any]) -> str:
    encoded = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).replace("</", "<\\/")
    html_text = _DASHBOARD_HTML.replace(
        "let state = null;",
        f"let state = null; window.__INITIAL_STATE__ = {encoded};",
        1,
    )
    return html_text.replace(
        "refresh();\n</script>",
        "if(window.__INITIAL_STATE__){ renderState(window.__INITIAL_STATE__); if(location.protocol==='file:'){ log('静态导出模式：初始状态可离线查看；参数编辑/实时比对请运行 tvol serve。'); } } else { refresh(); }\n</script>",
        1,
    )


__all__ = [
    "Dashboard",
    "create_dashboard_server",
    "export_static_dashboard",
    "project_boundaries",
    "serve_dashboard",
]
