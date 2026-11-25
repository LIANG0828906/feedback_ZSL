"use strict";

const state = { view: null, glyphID: null, tab: "geometry" };

const isWails = typeof window !== "undefined" && window.go && window.go.main;

async function apiInitial() {
  if (isWails) return window.go.main.App.Initial();
  const r = await fetch("/api/initial");
  if (!r.ok) throw new Error((await r.json()).error);
  return r.json();
}

async function apiParams(params) {
  if (isWails) return window.go.main.App.UpdateParams(params);
  const r = await fetch("/api/params", {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ params }),
  });
  return r.json();
}

async function apiDiagnose(kind, glyphID, segmentID) {
  if (isWails) return window.go.main.App.Diagnose(kind, glyphID, segmentID);
  const r = await fetch("/api/diagnose", {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ kind, glyph_id: glyphID, segment_id: segmentID }),
  });
  return r.json();
}

async function apiFullRender() {
  if (isWails) return window.go.main.App.FullRender();
  return null;
}

function currentGlyph() {
  return state.view.glyphs.find((g) => g.id === state.glyphID);
}

function init() {
  apiInitial().then((view) => {
    state.view = view;
    state.glyphID = view.glyphs[0].id;
    document.getElementById("algo").textContent = view.algorithm;
    fillParams(view.params);
    fillGlyphSelect();
    bindControls();
    renderAll();
  }).catch((e) => {
    document.body.insertAdjacentHTML("afterbegin",
      `<div style="color:#ff6b81;padding:12px">加载失败: ${e.message}</div>`);
  });
}

function fillParams(p) {
  const set = (id, v) => {
    const el = document.getElementById(id);
    el.value = v;
    document.getElementById("v" + id.slice(1)).textContent = v;
  };
  set("pWidth", p.width);
  set("pSpacing", p.spacing);
  set("pTolerance", p.tolerance);
  set("pScale", p.scale);
  set("pPadding", p.bbox_padding);
  document.getElementById("pJoin").value = p.join;
  document.getElementById("pCap").value = p.cap;
}

function readParams() {
  const num = (id) => parseFloat(document.getElementById(id).value);
  return {
    width: num("pWidth"), join: document.getElementById("pJoin").value,
    cap: document.getElementById("pCap").value, spacing: num("pSpacing"),
    tolerance: num("pTolerance"), scale: num("pScale"),
    bbox_padding: num("pPadding"),
  };
}

function fillGlyphSelect() {
  const sel = document.getElementById("glyphSelect");
  sel.innerHTML = "";
  state.view.glyphs.forEach((g) => {
    const o = document.createElement("option");
    o.value = g.id; o.textContent = g.id;
    sel.appendChild(o);
  });
  sel.value = state.glyphID;
}

function bindControls() {
  document.querySelectorAll("input[type=range]").forEach((el) => {
    el.addEventListener("input", () => {
      document.getElementById("v" + el.id.slice(1)).textContent = el.value;
    });
  });
  document.getElementById("glyphSelect").addEventListener("change", (e) => {
    state.glyphID = e.target.value; renderAll();
  });
  document.querySelectorAll(".tab").forEach((b) => {
    b.addEventListener("click", () => {
      document.querySelectorAll(".tab").forEach((x) => x.classList.remove("active"));
      b.classList.add("active");
      state.tab = b.dataset.tab;
      drawCanvas();
    });
  });
  document.getElementById("applyParams").addEventListener("click", applyParams);
  document.getElementById("reRender").addEventListener("click", async () => {
    await apiFullRender();
    const view = await apiInitial();
    state.view = view; renderAll();
  });
  document.querySelectorAll(".fault-grid button").forEach((b) => {
    b.addEventListener("click", () => injectFault(b.dataset.fault));
  });
}

async function applyParams() {
  const params = readParams();
  const res = await apiParams(params);
  state.view.params = params;
  if (res.error) {
    showFault(res.error, true);
    return;
  }
  showFault("增量重绘完成，与全量逐像素一致", false);
  if (res.invalidation) renderInvalidation(res.invalidation);
  const view = await apiInitial();
  state.view = view; renderAll();
}

async function injectFault(kind) {
  const seg = parseInt(document.getElementById("faultSegment").value, 10);
  const res = await apiDiagnose(kind, state.glyphID, seg);
  showFault(res.error || "已注入（未返回错误码）", !!res.error);
}

function showFault(msg, bad) {
  const el = document.getElementById("faultResult");
  el.textContent = msg;
  el.className = "fault-result " + (bad ? "bad" : "ok");
}

function renderInvalidation(entries) {
  const box = document.getElementById("invalidation");
  if (!entries || entries.length === 0) {
    box.innerHTML = '<p class="muted">无失效段。</p>';
    return;
  }
  box.innerHTML = entries.map((e) =>
    `<div class="inv-item ${e.downstream ? "down" : ""}">
      <strong>${e.glyph_id} #${e.segment_id}</strong><br/>
      ${e.reason} ${e.parameter ? "(" + e.parameter + ")" : ""}
    </div>`).join("");
}

function renderAll() {
  renderStateMatrix();
  renderTruth();
  drawCanvas();
}

function renderStateMatrix() {
  const g = currentGlyph();
  const box = document.getElementById("stateMatrix");
  box.innerHTML = g.segments.map((s) =>
    `<div class="cell ${s.state}" title="段 ${s.id} · ${s.contour_id} · ${s.state}">${s.id}</div>`
  ).join("");
}

function renderTruth() {
  const g = currentGlyph();
  const t = g.truth;
  document.getElementById("truth").innerHTML = `
    <table>
      <tr><th>闭合面积</th><td>${t.closed_area.toFixed(2)}</td></tr>
      <tr><th>包围盒</th><td>${t.bbox.min_x.toFixed(1)},${t.bbox.min_y.toFixed(1)} →
        ${t.bbox.max_x.toFixed(1)},${t.bbox.max_y.toFixed(1)}</td></tr>
      <tr><th>掩膜尺寸</th><td>${t.mask_width} × ${t.mask_height}</td></tr>
      <tr><th>笔画密度</th><td>${t.stroke_density}</td></tr>
      <tr><th>采样点</th><td>${t.sample_points.length}</td></tr>
    </table>`;
}

// Canvas projection: font units -> canvas pixels with uniform scale + padding.
function projector(g) {
  const b = g.bbox;
  const pad = 28;
  const w = 720 - pad * 2, h = 640 - pad * 2;
  const spanX = b.max_x - b.min_x, spanY = b.max_y - b.min_y;
  const k = Math.min(w / spanX, h / spanY);
  return (x, y) => ({
    x: pad + (x - b.min_x) * k,
    y: pad + (b.max_y - y) * k,
  });
}

function drawCanvas() {
  const g = currentGlyph();
  const cv = document.getElementById("stage");
  const ctx = cv.getContext("2d");
  ctx.clearRect(0, 0, cv.width, cv.height);
  const proj = projector(g);
  if (state.tab === "heat") return drawHeat(ctx, g);
  drawContours(ctx, g, proj);
  drawSkeleton(ctx, g, proj);
  if (state.tab === "stroke") drawBands(ctx, g, proj);
}

function pathFrom(ctx, pts, proj) {
  if (!pts || pts.length === 0) return;
  ctx.beginPath();
  pts.forEach((p, i) => {
    const q = proj(p.x, p.y);
    if (i === 0) ctx.moveTo(q.x, q.y); else ctx.lineTo(q.x, q.y);
  });
  ctx.closePath();
}

function drawContours(ctx, g, proj) {
  ctx.lineWidth = 1.2;
  g.contours.forEach((c) => {
    // Approximate contour control polygon for a light reference layer.
    ctx.beginPath();
    c.segments.forEach((s, i) => {
      const p = proj(s.p0.x, s.p0.y);
      if (i === 0) ctx.moveTo(p.x, p.y); else ctx.lineTo(p.x, p.y);
    });
    ctx.closePath();
    ctx.strokeStyle = "rgba(142,160,189,0.55)";
    ctx.setLineDash([5, 4]);
    ctx.stroke();
    ctx.setLineDash([]);
  });
}

function drawSkeleton(ctx, g, proj) {
  Object.values(g.skeleton).forEach((pts) => {
    pathFrom(ctx, pts, proj);
    ctx.strokeStyle = "#56c2ff";
    ctx.lineWidth = 1.6;
    ctx.stroke();
  });
  // Segment boundaries and sample points.
  g.segments.forEach((s) => {
    (s.skeleton_polyline || []).forEach((p) => {
      const q = proj(p.x, p.y);
      ctx.fillStyle = "#8d7bff";
      ctx.beginPath(); ctx.arc(q.x, q.y, 2.2, 0, Math.PI * 2); ctx.fill();
    });
  });
}

function drawBands(ctx, g, proj) {
  g.segments.forEach((s, idx) => {
    if (!s.band_outline || !s.band_outline.points) return;
    pathFrom(ctx, s.band_outline.points, proj);
    ctx.fillStyle = idx % 2 ? "rgba(141,123,255,0.18)" : "rgba(86,194,255,0.18)";
    ctx.fill();
    ctx.strokeStyle = "rgba(255,200,87,0.8)";
    ctx.lineWidth = 1;
    ctx.stroke();
  });
}

function drawHeat(ctx, g) {
  const img = new Image();
  img.onload = () => {
    // Draw the grayscale mask with a thermal lookup table.
    const off = document.createElement("canvas");
    off.width = img.width; off.height = img.height;
    const octx = off.getContext("2d");
    octx.drawImage(img, 0, 0);
    const data = octx.getImageData(0, 0, off.width, off.height);
    const out = octx.createImageData(off.width, off.height);
    for (let i = 0; i < data.data.length; i += 4) {
      const v = data.data[i];
      const [r, gg, b] = thermal(v);
      out.data[i] = r; out.data[i + 1] = gg; out.data[i + 2] = b; out.data[i + 3] = 255;
    }
    octx.putImageData(out, 0, 0);
    ctx.imageSmoothingEnabled = false;
    ctx.drawImage(off, 20, 20, 680, 600);
  };
  img.src = g.mask_data_url;
}

function thermal(v) {
  // 0 -> deep blue, 128 -> magenta, 255 -> hot yellow.
  const t = v / 255;
  const r = Math.round(255 * Math.min(1, t * 1.6));
  const gg = Math.round(255 * Math.max(0, t - 0.4) * 1.4);
  const b = Math.round(255 * (1 - t) * 0.9);
  return [r, gg, b];
}

document.addEventListener("DOMContentLoaded", init);
