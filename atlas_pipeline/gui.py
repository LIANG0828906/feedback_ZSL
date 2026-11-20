from __future__ import annotations

import json
import mimetypes
import threading
import webbrowser
from dataclasses import dataclass
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlparse

from .codec import read_atlas
from .errors import AtlasError, ErrorCode
from .export import export_published
from .opengl_preview import OpenGLInfo, check_opengl, render_rgba_opengl
from .pipeline import AtlasPipeline, ConversionJob
from .render import encode_png_rgb, preview_rgba_float, preview_rgb, scale_nearest


@dataclass
class GUIState:
    pipeline: AtlasPipeline
    export_dir: Path
    renderer: str
    opengl: OpenGLInfo
    lock: threading.RLock
    last_job: dict[str, Any] | None = None
    last_job_ref: ConversionJob | None = None


HTML = r"""<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>HDR Atlas Studio</title>
<style>
:root { color-scheme: dark; --bg:#101318; --panel:#191e26; --line:#303846;
  --text:#e6ebf2; --muted:#92a0b3; --accent:#5cc8ff; --good:#57d68d;
  --warn:#ffc857; --bad:#ff6b6b; }
* { box-sizing: border-box; }
body { margin:0; background:var(--bg); color:var(--text);
  font:14px/1.45 ui-sans-serif, system-ui, "Segoe UI", sans-serif; }
header { display:flex; gap:16px; align-items:baseline; padding:12px 18px;
  border-bottom:1px solid var(--line); background:#141922; }
header h1 { margin:0; font-size:18px; letter-spacing:.02em; }
header span { color:var(--muted); }
main { display:grid; grid-template-columns:minmax(560px,1fr) 350px;
  gap:14px; padding:14px; align-items:start; }
.panel { background:var(--panel); border:1px solid var(--line); border-radius:10px; }
.viewport { min-width:0; overflow:hidden; }
.toolbar { display:flex; gap:8px; align-items:center; flex-wrap:wrap;
  padding:10px; border-bottom:1px solid var(--line); }
.canvas-wrap { position:relative; background:
  repeating-conic-gradient(#161b23 0 25%, #12171e 0 50%) 50% / 20px 20px; }
canvas { display:block; width:100%; height:auto; min-height:460px; cursor:crosshair; }
#status { flex:1; min-width:160px; color:var(--muted); }
#status.error { color:var(--bad); }
#status.ok { color:var(--good); }
.aside { padding:12px; }
.aside h2 { margin:4px 0 8px; font-size:13px; color:var(--muted);
  text-transform:uppercase; letter-spacing:.08em; }
.grid2 { display:grid; grid-template-columns:1fr 1fr; gap:8px; }
label.field { display:grid; gap:3px; color:var(--muted); font-size:12px; }
input, select, button { font:inherit; }
input, select { background:#0e1218; color:var(--text); border:1px solid var(--line);
  border-radius:6px; padding:5px 7px; min-width:0; }
button { background:#243141; border:1px solid #3a4a5e; color:var(--text);
  padding:6px 10px; border-radius:6px; cursor:pointer; }
button:hover { border-color:var(--accent); }
button.primary { background:#134a65; border-color:#1c7da8; }
button.danger { background:#4a2025; border-color:#8b3941; }
button:disabled { opacity:.42; cursor:not-allowed; }
.tile-list { display:grid; grid-template-columns:repeat(4,1fr); gap:6px; }
.tile-button { padding:7px 4px; font-size:12px; }
.tile-button.active { outline:2px solid var(--accent); background:#253646; }
.tile-button b { display:block; font-size:13px; }
.tile-button small { color:var(--muted); }
.timeline { margin:0 14px 14px; padding:10px 12px; }
.timeline-head { display:flex; justify-content:space-between; color:var(--muted); }
.version-list { display:flex; gap:8px; overflow-x:auto; padding-top:8px; }
.version { min-width:104px; text-align:left; }
.version.active { border-color:var(--accent); box-shadow:0 0 0 1px var(--accent); }
.version small { display:block; color:var(--muted); }
pre { margin:12px 0 0; padding:9px; max-height:170px; overflow:auto;
  background:#0d1117; border:1px solid var(--line); border-radius:6px;
  color:#b8c5d6; font:12px/1.4 ui-monospace, "Cascadia Mono", monospace;
  white-space:pre-wrap; }
.legend { display:flex; gap:10px; align-items:center; color:var(--muted);
  font-size:12px; padding:7px 10px; border-top:1px solid var(--line); }
.swatch { width:64px; height:8px; border-radius:4px;
  background:linear-gradient(90deg,#000,#f00,#ff0,#fff); }
.metrics { display:grid; gap:4px; color:var(--muted); font-size:12px; }
.metrics b { color:var(--text); float:right; }
@media (max-width:900px) { main { grid-template-columns:1fr; } }
</style>
</head>
<body>
<header>
  <h1>HDR Atlas Studio</h1>
  <span id="atlasMeta"></span>
  <span id="rendererMeta"></span>
</header>
<main>
  <section class="panel viewport">
    <div class="toolbar">
      <button id="submitBtn" class="primary">提交转换 (S)</button>
      <button id="cancelBtn" class="danger">取消任务</button>
      <button id="rollbackBtn">回退到查看版本</button>
      <button id="exportBtn">导出图集 + 报告</button>
      <label><input id="heatToggle" type="checkbox"> 误差热力</label>
      <span id="status">就绪</span>
    </div>
    <div class="canvas-wrap">
      <canvas id="preview" width="960" height="620"></canvas>
    </div>
    <div class="legend"><span class="swatch"></span><span>区块最大码值偏差（按当前版本峰值归一化）</span></div>
  </section>
  <aside class="panel aside">
    <h2>共享转换链</h2>
    <div class="grid2">
      <label class="field">目标色彩空间
        <select id="targetSpace">
          <option value="rec709">Rec.709</option>
          <option value="display-p3">Display-P3</option>
          <option value="rec2020">Rec.2020</option>
        </select>
      </label>
      <label class="field">目标传递函数
        <select id="targetTransfer">
          <option value="linear">线性</option>
          <option value="srgb">sRGB</option>
          <option value="pq">PQ (ST 2084)</option>
        </select>
      </label>
      <label class="field">峰值亮度 (nit)
        <input id="targetPeak" type="number" min="1" max="10000" step="50">
      </label>
      <label class="field">白点适应
        <select id="targetWhite">
          <option value="D65">D65</option>
          <option value="D50">D50</option>
        </select>
      </label>
    </div>
    <h2>区块参数 <span id="selectedLabel"></span></h2>
    <div class="tile-list" id="tileList"></div>
    <div class="grid2" style="margin-top:10px">
      <label class="field">曝光 EV
        <input id="tileExposure" type="number" min="-64" max="64" step="0.05">
      </label>
      <label class="field">色调映射
        <select id="tileCurve">
          <option value="none">不压缩</option>
          <option value="reinhard">Reinhard</option>
          <option value="hable">Hable</option>
        </select>
      </label>
      <label class="field">映射强度
        <input id="tileStrength" type="number" min="0" max="1" step="0.05">
      </label>
    </div>
    <h2>误差摘要</h2>
    <div class="metrics" id="metrics"></div>
    <pre id="log">点击预览选择区块；修改参数后提交。</pre>
  </aside>
</main>
<section class="panel timeline">
  <div class="timeline-head"><b>版本时间轴</b><span>点击查看历史；回退会生成新的单调递增版本</span></div>
  <div class="version-list" id="versionList"></div>
</section>
<script>
let state = null;
let viewVersion = null;
let selectedTile = 0;
let previewImage = new Image();
let imageRect = null;
let polling = null;

const $ = (id) => document.getElementById(id);
const colorSpaces = ["rec709","display-p3","rec2020"];
const transfers = ["linear","srgb","pq"];
const curves = ["none","reinhard","hable"];

function setStatus(text, kind="") {
  const el = $("status");
  el.textContent = text;
  el.className = kind;
}

function errorText(error) {
  if (!error) return "未知错误";
  let text = error.code + ": " + error.message;
  if (error.field) text += " [field=" + error.field + "]";
  if (error.tile !== undefined && error.tile !== null) text += " [tile=" + error.tile + "]";
  return text;
}

async function api(path, options={}) {
  const response = await fetch(path, options);
  const data = await response.json();
  if (!response.ok || data.ok === false) {
    throw new Error(JSON.stringify(data.error || data));
  }
  return data;
}

function parseError(exc) {
  try {
    const text = String(exc.message || exc);
    const parsed = JSON.parse(text);
    return errorText(parsed.error || parsed);
  } catch (_) {
    return String(exc);
  }
}

async function loadState(version=null) {
  const q = version === null ? "" : "?version=" + encodeURIComponent(version);
  const data = await api("/api/state" + q);
  state = data;
  if (viewVersion === null) viewVersion = data.current_version;
  if (version === null) viewVersion = data.current_version;
  selectedTile = Math.max(0, Math.min(selectedTile, data.tiles.length - 1));
  $("atlasMeta").textContent =
    `${data.atlas.width}×${data.atlas.height} · ${data.atlas.bit_depth}-bit · ` +
    `${data.atlas.grid_cols}×${data.atlas.grid_rows} tiles`;
  $("rendererMeta").textContent = data.opengl.available
    ? `OpenGL: ${data.opengl.renderer}`
    : "OpenGL context unavailable; CPU fallback";
  $("targetSpace").value = data.config.target_color_space;
  $("targetTransfer").value = data.config.target_transfer;
  $("targetPeak").value = data.config.target_peak_nits;
  $("targetWhite").value = data.config.target_white_point;
  renderVersionList();
  renderTiles();
  loadSelectedTile();
  loadPreview();
  updateJobUi();
}

function renderVersionList() {
  const root = $("versionList");
  root.innerHTML = "";
  for (const version of state.versions) {
    const button = document.createElement("button");
    button.className = "version" + (version.number === viewVersion ? " active" : "");
    button.innerHTML = `<b>v${version.number}</b>` +
      `<small>${version.reason}</small>` +
      `<small>影响 ${version.affected_tiles.length} 区块</small>` +
      `<small>${version.content_hash.slice(0, 10)}…</small>`;
    button.onclick = () => { viewVersion = version.number; loadState(version.number); };
    root.appendChild(button);
  }
}

function renderTiles() {
  const root = $("tileList");
  root.innerHTML = "";
  state.tiles.forEach((tile, index) => {
    const button = document.createElement("button");
    button.className = "tile-button" + (index === selectedTile ? " active" : "");
    button.innerHTML = `<b>#${index}</b><small>EV ${tile.params.exposure_ev}</small>` +
      `<small>${tile.params.tone_curve}</small>`;
    button.onclick = () => { selectedTile = index; renderTiles(); loadSelectedTile(); drawPreview(); };
    root.appendChild(button);
  });
}

function loadSelectedTile() {
  const tile = state.tiles[selectedTile];
  $("selectedLabel").textContent = "· #" + selectedTile;
  $("tileExposure").value = tile.params.exposure_ev;
  $("tileCurve").value = tile.params.tone_curve;
  $("tileStrength").value = tile.params.tone_strength;
  $("metrics").innerHTML =
    `<div>最大码值偏差 <b>${tile.summary.max_code_deviation.toFixed(5)}</b></div>` +
    `<div>RMSE <b>${tile.summary.rmse_code_deviation.toFixed(5)}</b></div>` +
    `<div>P99 <b>${tile.summary.p99_code_deviation.toFixed(5)}</b></div>` +
    `<div>裁切像素比例 <b>${(tile.summary.clipped_fraction*100).toFixed(3)}%</b></div>` +
    `<div>区块哈希 <b>${tile.tile_hash.slice(0, 12)}…</b></div>`;
}

function loadPreview() {
  const version = state.versions.find(v => v.number === viewVersion) || state.current_version_record;
  if (!version) return;
  const heat = $("heatToggle").checked ? 1 : 0;
  const url = `/api/preview.png?version=${viewVersion}&heatmap=${heat}` +
    `&hash=${encodeURIComponent(version.content_hash)}`;
  previewImage = new Image();
  previewImage.onload = () => drawPreview();
  previewImage.src = url;
}

function drawPreview() {
  const canvas = $("preview");
  const ctx = canvas.getContext("2d");
  const cw = canvas.width, ch = canvas.height;
  ctx.clearRect(0, 0, cw, ch);
  if (!previewImage.complete || !previewImage.naturalWidth) return;
  const scale = Math.min(cw / previewImage.naturalWidth, ch / previewImage.naturalHeight);
  const w = previewImage.naturalWidth * scale;
  const h = previewImage.naturalHeight * scale;
  const x = (cw - w) / 2, y = (ch - h) / 2;
  ctx.imageSmoothingEnabled = false;
  ctx.drawImage(previewImage, x, y, w, h);
  imageRect = {x, y, w, h};
  ctx.lineWidth = 1;
  ctx.strokeStyle = "rgba(255,255,255,.36)";
  for (const tile of state.tiles) {
    const [x0,y0,x1,y1] = tile.bbox;
    const tx = x + x0 / state.atlas.width * w;
    const ty = y + y0 / state.atlas.height * h;
    const tw = (x1-x0) / state.atlas.width * w;
    const th = (y1-y0) / state.atlas.height * h;
    ctx.strokeRect(tx, ty, tw, th);
    ctx.fillStyle = "rgba(5,10,15,.68)";
    ctx.fillRect(tx+3, ty+3, 38, 17);
    ctx.fillStyle = "#e6ebf2";
    ctx.font = "12px ui-monospace, monospace";
    ctx.fillText("#" + tile.index, tx+8, ty+16);
  }
  const selected = state.tiles[selectedTile];
  if (selected) {
    const [x0,y0,x1,y1] = selected.bbox;
    ctx.lineWidth = 3;
    ctx.strokeStyle = "#5cc8ff";
    ctx.strokeRect(
      x + x0/state.atlas.width*w,
      y + y0/state.atlas.height*h,
      (x1-x0)/state.atlas.width*w,
      (y1-y0)/state.atlas.height*h
    );
  }
}

$("preview").addEventListener("click", (event) => {
  if (!imageRect || !state) return;
  const canvas = $("preview");
  const rect = canvas.getBoundingClientRect();
  const px = (event.clientX - rect.left) * canvas.width / rect.width;
  const py = (event.clientY - rect.top) * canvas.height / rect.height;
  if (px < imageRect.x || py < imageRect.y ||
      px > imageRect.x + imageRect.w || py > imageRect.y + imageRect.h) return;
  const u = (px - imageRect.x) / imageRect.w;
  const v = (py - imageRect.y) / imageRect.h;
  const x = Math.min(state.atlas.width - 1, Math.floor(u * state.atlas.width));
  const y = Math.min(state.atlas.height - 1, Math.floor(v * state.atlas.height));
  for (const tile of state.tiles) {
    const [x0,y0,x1,y1] = tile.bbox;
    if (x >= x0 && x < x1 && y >= y0 && y < y1) {
      selectedTile = tile.index;
      renderTiles();
      loadSelectedTile();
      drawPreview();
      return;
    }
  }
});

$("heatToggle").onchange = loadPreview;

async function submitConversion() {
  if (!state || viewVersion !== state.current_version) {
    setStatus("请先回退到历史版本，再基于回退版本提交", "error");
    return;
  }
  const config = {
    target_color_space: $("targetSpace").value,
    target_transfer: $("targetTransfer").value,
    target_peak_nits: Number($("targetPeak").value),
    source_white_point: state.config.source_white_point,
    target_white_point: $("targetWhite").value,
    adaptation: state.config.adaptation,
  };
  const tile = {
    exposure_ev: Number($("tileExposure").value),
    tone_curve: $("tileCurve").value,
    tone_strength: Number($("tileStrength").value),
  };
  setStatus("提交转换…");
  try {
    const result = await api("/api/submit", {
      method:"POST", headers:{"Content-Type":"application/json"},
      body: JSON.stringify({base_version: state.current_version, config, tile_updates: {[selectedTile]: tile}})
    });
    setStatus(`已接受，目标版本 v${result.target_version}`, "ok");
    startPolling();
  } catch (exc) {
    setStatus(parseError(exc), "error");
  }
}

async function cancelConversion() {
  try {
    const result = await api("/api/cancel", {method:"POST"});
    setStatus(result.cancelled ? "已请求取消" : "当前没有转换任务", result.cancelled ? "ok" : "");
  } catch (exc) { setStatus(parseError(exc), "error"); }
}

async function rollbackToView() {
  if (!state || viewVersion === state.current_version) {
    setStatus("请选择历史版本再回退");
    return;
  }
  try {
    const result = await api("/api/rollback", {
      method:"POST", headers:{"Content-Type":"application/json"},
      body: JSON.stringify({version: viewVersion})
    });
    setStatus(`已发布回退版本 v${result.current_version}`, "ok");
    await loadState();
  } catch (exc) { setStatus(parseError(exc), "error"); }
}

async function exportCurrent() {
  try {
    const result = await api("/api/export", {method:"POST"});
    setStatus(`已导出 v${result.version}`, "ok");
    $("log").textContent =
      `atlas: ${result.atlas_url}\nreport: ${result.report_url}\n` +
      `content: ${result.content_hash}\nreport hash: ${result.report_sha256}`;
  } catch (exc) { setStatus(parseError(exc), "error"); }
}

function updateJobUi() {
  const job = state.job;
  $("cancelBtn").disabled = !(job && job.active);
  if (!job) return;
  if (job.error) {
    setStatus(errorText(job.error), "error");
    return;
  }
  if (job.active) {
    setStatus(`转换 v${job.target_version}: ${job.completed}/${job.affected.length} 区块…`);
  } else if (job.status === "published") {
    setStatus(`版本 v${job.target_version} 已整体发布`, "ok");
  }
}

function startPolling() {
  if (polling) return;
  polling = setInterval(async () => {
    try {
      const job = await api("/api/job");
      if (!state) return;
      state.job = job.job;
      updateJobUi();
      if (!job.job || !job.job.active) {
        clearInterval(polling); polling = null;
        await loadState();
      }
    } catch (exc) {
      clearInterval(polling); polling = null;
      setStatus(parseError(exc), "error");
    }
  }, 350);
}

$("submitBtn").onclick = submitConversion;
$("cancelBtn").onclick = cancelConversion;
$("rollbackBtn").onclick = rollbackToView;
$("exportBtn").onclick = exportCurrent;
window.addEventListener("keydown", (event) => {
  if (event.target.matches("input,select,textarea")) return;
  if (event.key === "s") submitConversion();
  if (event.key === "h") { $("heatToggle").checked = !$("heatToggle").checked; loadPreview(); }
});
loadState().catch(exc => setStatus(parseError(exc), "error"));
</script>
</body>
</html>
"""


def _version_state(pipeline: AtlasPipeline, number: int) -> dict[str, Any]:
    with pipeline._publish_lock:
        version = pipeline.get_version(number)
    atlas = pipeline.atlas
    return {
        "number": version.number,
        "parent_version": version.parent_version,
        "chain_hash": version.chain_hash,
        "content_hash": version.content_hash,
        "reason": version.reason,
        "rollback_of": version.rollback_of,
        "affected_tiles": list(version.affected_tiles),
        "reused_tiles": list(version.reused_tiles),
    }


def _job_document(
    pipeline: AtlasPipeline,
    state: GUIState,
) -> dict[str, Any] | None:
    job = pipeline.active_job
    if job is not None:
        return {
            "active": True,
            "status": job.status,
            "target_version": job.version_number,
            "completed": job.completed_count,
            "affected": list(job.affected_tiles),
            "error": None,
        }
    with state.lock:
        if state.last_job is None:
            return None
        document = dict(state.last_job)
        job = state.last_job_ref
        if job is not None:
            if job.error is not None:
                document.update(
                    {
                        "active": False,
                        "status": job.status,
                        "error": job.error.to_dict(),
                        "completed": job.completed_count,
                    }
                )
            elif job.status == "published" or (
                document.get("target_version") is not None
                and pipeline.current_version_number >= int(document["target_version"])
            ):
                document.update(
                    {
                        "active": False,
                        "status": "published",
                        "completed": len(job.affected_tiles),
                        "error": None,
                    }
                )
        state.last_job = dict(document)
        return document


def _state_document(
    state: GUIState,
    version_number: int | None,
) -> dict[str, Any]:
    pipeline = state.pipeline
    with pipeline._publish_lock:
        versions = pipeline._versions
        current = pipeline._current_version
        number = current if version_number is None else int(version_number)
        try:
            version = pipeline._versions[number - 1]
        except IndexError:
            raise AtlasError(
                ErrorCode.E_VERSION_STALE,
                f"version {number} does not exist",
                field="version",
            ) from None
    atlas = pipeline.atlas
    tiles = []
    for spec, params, summary, digest in zip(
        atlas.tiles,
        version.tile_params,
        version.tile_summaries,
        version.output_tile_hashes,
    ):
        tiles.append(
            {
                "index": spec.index,
                "bbox": list(spec.bbox),
                "params": params.to_dict(),
                "summary": summary.to_dict(),
                "tile_hash": digest,
            }
        )
    return {
        "current_version": current,
        "view_version": number,
        "current_version_record": _version_state(pipeline, current),
        "versions": [_version_state(pipeline, item.number) for item in versions],
        "config": version.config.to_dict(),
        "tile_params": [item.to_dict() for item in version.tile_params],
        "tiles": tiles,
        "atlas": {
            "width": atlas.header.width,
            "height": atlas.header.height,
            "bit_depth": atlas.header.bit_depth,
            "color_space": atlas.header.color_space,
            "transfer_function": atlas.header.transfer_function,
            "source_peak_nits": atlas.header.source_peak_nits,
            "grid_cols": atlas.header.grid_cols,
            "grid_rows": atlas.header.grid_rows,
            "source_hash": atlas.source_hash,
            "fingerprint": atlas.fingerprint,
        },
        "opengl": state.opengl.to_dict(),
        "job": _job_document(pipeline, state),
    }


def _error_status(error: AtlasError) -> HTTPStatus:
    if error.code in (ErrorCode.E_BUSY, ErrorCode.E_VERSION_STALE):
        return HTTPStatus.CONFLICT
    if error.code in (ErrorCode.E_PARAM_CONFLICT, ErrorCode.E_USAGE):
        return HTTPStatus.BAD_REQUEST
    return HTTPStatus.INTERNAL_SERVER_ERROR


def _handler_class(state: GUIState):
    class Handler(BaseHTTPRequestHandler):
        server_version = "AtlasGUI/1.0"

        def log_message(self, format: str, *args: Any) -> None:
            return

        def _send_json(self, status: HTTPStatus, payload: Any) -> None:
            body = json.dumps(payload, ensure_ascii=False, sort_keys=True).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)

        def _send_bytes(
            self,
            status: HTTPStatus,
            body: bytes,
            content_type: str,
        ) -> None:
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)

        def _read_json(self) -> dict[str, Any]:
            length = int(self.headers.get("Content-Length", "0"))
            if length > 2 * 1024 * 1024:
                raise AtlasError(
                    ErrorCode.E_USAGE,
                    "request body is too large",
                    field="Content-Length",
                )
            raw = self.rfile.read(length)
            if not raw:
                return {}
            try:
                value = json.loads(raw.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                raise AtlasError(
                    ErrorCode.E_USAGE,
                    f"request JSON is invalid: {exc}",
                    field="body",
                ) from None
            if not isinstance(value, dict):
                raise AtlasError(
                    ErrorCode.E_USAGE,
                    "request JSON root must be an object",
                    field="body",
                )
            return value

        def do_GET(self) -> None:  # noqa: N802
            try:
                parsed = urlparse(self.path)
                if parsed.path == "/":
                    self._send_bytes(
                        HTTPStatus.OK,
                        HTML.encode("utf-8"),
                        "text/html; charset=utf-8",
                    )
                    return
                if parsed.path == "/api/state":
                    query = parse_qs(parsed.query)
                    raw_version = query.get("version", [None])[0]
                    payload = _state_document(
                        state,
                        None if raw_version in (None, "") else int(raw_version),
                    )
                    payload["ok"] = True
                    self._send_json(HTTPStatus.OK, payload)
                    return
                if parsed.path == "/api/preview.png":
                    query = parse_qs(parsed.query)
                    raw_version = query.get("version", [None])[0]
                    heatmap = query.get("heatmap", ["0"])[0] == "1"
                    version = (
                        state.pipeline.current_version_number
                        if raw_version in (None, "")
                        else int(raw_version)
                    )
                    preview = state.pipeline.get_preview(version)
                    if state.renderer == "opengl":
                        try:
                            rgba = render_rgba_opengl(
                                preview_rgba_float(preview, heatmap=heatmap)
                            )
                            rgb = rgba[..., :3]
                        except Exception:
                            rgb = preview_rgb(preview, heatmap=heatmap)
                    else:
                        rgb = preview_rgb(preview, heatmap=heatmap)
                    scale = max(
                        1,
                        min(
                            8,
                            720 // max(preview.srgb.shape[0], preview.srgb.shape[1]),
                        ),
                    )
                    png = encode_png_rgb(scale_nearest(rgb, scale))
                    self._send_bytes(HTTPStatus.OK, png, "image/png")
                    return
                if parsed.path == "/api/job":
                    self._send_json(
                        HTTPStatus.OK,
                        {"ok": True, "job": _job_document(state.pipeline, state)},
                    )
                    return
                if parsed.path == "/api/trace":
                    trace = state.pipeline.trace_document()
                    trace["ok"] = True
                    self._send_json(HTTPStatus.OK, trace)
                    return
                if parsed.path.startswith("/files/"):
                    name = parsed.path[len("/files/") :]
                    if (
                        not name
                        or "/" in name
                        or "\\" in name
                        or name in (".", "..")
                    ):
                        raise AtlasError(
                            ErrorCode.E_USAGE,
                            "invalid file name",
                            field="path",
                        )
                    path = (state.export_dir / name).resolve()
                    if not path.is_file() or state.export_dir.resolve() not in path.parents:
                        raise AtlasError(
                            ErrorCode.E_IO,
                            "exported file does not exist",
                            field="path",
                        )
                    content_type = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
                    self._send_bytes(HTTPStatus.OK, path.read_bytes(), content_type)
                    return
                raise AtlasError(
                    ErrorCode.E_USAGE,
                    "unknown route",
                    field="path",
                )
            except AtlasError as error:
                self._send_json(
                    _error_status(error),
                    {"ok": False, "error": error.to_dict()},
                )
            except Exception as exc:
                self._send_json(
                    HTTPStatus.INTERNAL_SERVER_ERROR,
                    {
                        "ok": False,
                        "error": {
                            "code": "E_USAGE",
                            "message": f"unexpected GUI error: {exc}",
                        },
                    },
                )

        def do_POST(self) -> None:  # noqa: N802
            try:
                parsed = urlparse(self.path)
                body = self._read_json()
                if parsed.path == "/api/submit":
                    expected = body.get("base_version")
                    config = body.get("config") or {}
                    updates = body.get("tile_updates") or {}
                    if not isinstance(config, dict):
                        raise AtlasError(
                            ErrorCode.E_PARAM_CONFLICT,
                            "config must be an object",
                            field="config",
                        )
                    if not isinstance(updates, dict):
                        raise AtlasError(
                            ErrorCode.E_PARAM_CONFLICT,
                            "tile_updates must be an object",
                            field="tile_updates",
                        )
                    normalized_updates: dict[int, Any] = {}
                    for key, value in updates.items():
                        try:
                            normalized_updates[int(key)] = value
                        except (TypeError, ValueError):
                            raise AtlasError(
                                ErrorCode.E_PARAM_CONFLICT,
                                "tile update key must be an integer",
                                field="tile_updates",
                            ) from None
                    job = state.pipeline.submit_async(
                        config=config,
                        tile_updates=normalized_updates,
                        expected_version=None if expected is None else int(expected),
                    )
                    with state.lock:
                        state.last_job = {
                            "active": True,
                            "status": job.status,
                            "target_version": job.version_number,
                            "completed": 0,
                            "affected": list(job.affected_tiles),
                            "error": None,
                        }
                        state.last_job_ref = job
                    self._send_json(
                        HTTPStatus.ACCEPTED,
                        {
                            "ok": True,
                            "target_version": job.version_number,
                            "affected_tiles": list(job.affected_tiles),
                        },
                    )
                    return
                if parsed.path == "/api/cancel":
                    cancelled = state.pipeline.cancel_current()
                    self._send_json(
                        HTTPStatus.OK,
                        {"ok": True, "cancelled": cancelled},
                    )
                    return
                if parsed.path == "/api/rollback":
                    number = int(body["version"])
                    version = state.pipeline.rollback(number)
                    self._send_json(
                        HTTPStatus.OK,
                        {
                            "ok": True,
                            "current_version": version.number,
                            "rollback_of": version.rollback_of,
                        },
                    )
                    return
                if parsed.path == "/api/export":
                    if state.pipeline.active_job is not None:
                        raise AtlasError(
                            ErrorCode.E_BUSY,
                            "cannot export while a conversion task is active",
                        )
                    version = state.pipeline.current_version_number
                    atlas_name = f"v{version}.atlas"
                    report_name = f"v{version}.report.json"
                    trace_name = "trace.json"
                    result = export_published(
                        state.pipeline,
                        state.export_dir / atlas_name,
                        state.export_dir / report_name,
                        expected_version=version,
                    )
                    state.pipeline.write_trace(state.export_dir / trace_name)
                    self._send_json(
                        HTTPStatus.OK,
                        {
                            "ok": True,
                            **result.to_dict(),
                            "atlas_url": f"/files/{atlas_name}",
                            "report_url": f"/files/{report_name}",
                            "trace_url": f"/files/{trace_name}",
                        },
                    )
                    return
                raise AtlasError(
                    ErrorCode.E_USAGE,
                    "unknown route",
                    field="path",
                )
            except AtlasError as error:
                if error.code not in (ErrorCode.E_BUSY,):
                    with state.lock:
                        state.last_job = {
                            "active": False,
                            "status": "rejected",
                            "target_version": None,
                            "completed": 0,
                            "affected": [],
                            "error": error.to_dict(),
                        }
                self._send_json(
                    _error_status(error),
                    {"ok": False, "error": error.to_dict()},
                )
            except Exception as exc:
                self._send_json(
                    HTTPStatus.INTERNAL_SERVER_ERROR,
                    {
                        "ok": False,
                        "error": {
                            "code": "E_USAGE",
                            "message": f"unexpected GUI error: {exc}",
                        },
                    },
                )

    return Handler


def create_server(
    atlas_path: str | Path,
    *,
    host: str = "127.0.0.1",
    port: int = 8765,
    export_dir: str | Path = "atlas-export",
    renderer: str = "cpu",
) -> ThreadingHTTPServer:
    pipeline = AtlasPipeline(read_atlas(atlas_path))
    opengl = check_opengl()
    chosen_renderer = renderer
    if chosen_renderer == "auto":
        chosen_renderer = "opengl" if opengl.available else "cpu"
    if chosen_renderer == "opengl" and not opengl.available:
        chosen_renderer = "cpu"
    export_path = Path(export_dir)
    export_path.mkdir(parents=True, exist_ok=True)
    state = GUIState(
        pipeline=pipeline,
        export_dir=export_path,
        renderer=chosen_renderer,
        opengl=opengl,
        lock=threading.RLock(),
        last_job_ref=None,
    )
    pipeline.set_checkpoint_path(export_path / "studio.snapshot.json")
    pipeline.save_snapshot(export_path / "studio.snapshot.json")
    server = ThreadingHTTPServer((host, int(port)), _handler_class(state))
    server.state = state  # type: ignore[attr-defined]
    return server


def run_gui(
    atlas_path: str | Path,
    *,
    host: str = "127.0.0.1",
    port: int = 8765,
    export_dir: str | Path = "atlas-export",
    renderer: str = "cpu",
    open_browser: bool = True,
) -> None:
    server = create_server(
        atlas_path,
        host=host,
        port=port,
        export_dir=export_dir,
        renderer=renderer,
    )
    url_host = "127.0.0.1" if host in ("0.0.0.0", "::") else host
    url = f"http://{url_host}:{server.server_port}/"
    print(f"HDR Atlas Studio: {url}")
    if open_browser:
        threading.Thread(target=lambda: webbrowser.open(url), daemon=True).start()
    try:
        server.serve_forever(poll_interval=0.25)
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
