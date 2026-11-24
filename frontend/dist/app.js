(() => {
  "use strict";

  const state = {
    ws: null,
    camera: {
      yaw: -1.4, pitch: 0.8, distance: 10,
      targetX: 0, targetY: 0, targetZ: -0.3,
      fov: 37, width: 720, height: 480,
    },
    clip: { enabled: false, axis: "z", value: 0, keepAbove: true },
    dragging: false,
    dragMode: "orbit",
    lastPointer: null,
    lastApiPreview: 0,
    drawMs: 0,
    mode: "exact",
    busy: false,
    settleTimer: null,
  };

  const $ = (id) => document.getElementById(id);
  const fmt = (v, n = 0) => Number(v || 0).toLocaleString("en-US", { maximumFractionDigits: n, minimumFractionDigits: n });
  const fmtBytes = (n) => {
    n = Number(n || 0);
    if (n > 1024 * 1024) return (n / (1024 * 1024)).toFixed(2) + " MB";
    if (n > 1024) return (n / 1024).toFixed(1) + " KB";
    return n + " B";
  };

  async function dispatch(action, payload = {}) {
    const wails = window.go?.main?.DesktopApp?.Dispatch || window.go?.main?.App?.Dispatch;
    if (wails) {
      try {
        const raw = await wails(action, JSON.stringify(payload));
        return typeof raw === "string" ? JSON.parse(raw) : raw;
      } catch (err) {
        return { ok: false, error: { code: "E_RUNTIME", message: String(err) } };
      }
    }
    try {
      const res = await fetch(`/api/dispatch?action=${encodeURIComponent(action)}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload || {}),
      });
      return await res.json();
    } catch (err) {
      return { ok: false, error: { code: "E_NETWORK", message: String(err) } };
    }
  }

  function notify(title, detail = "", level = "info", ttl = 9000) {
    const box = $("toasts");
    const node = document.createElement("div");
    node.className = `toast ${level}`;
    const strong = document.createElement("strong");
    strong.textContent = title;
    node.appendChild(strong);
    if (detail) node.appendChild(document.createTextNode("  " + detail));
    box.prepend(node);
    while (box.children.length > 4) box.lastChild.remove();
    if (ttl > 0) window.setTimeout(() => node.remove(), ttl);
  }

  function showError(err) {
    if (!err) return;
    setStatus("ERROR", "error");
    const suffix = err.path ? ` @ ${err.path}` : "";
    const offset = Number.isFinite(err.offset) && err.offset > 0 ? ` +${err.offset}` : "";
    notify(err.code || "ERROR", (err.message || "unknown error") + suffix + offset, "error", 12000);
  }

  function setStatus(label, cls = "") {
    $("statusLabel").textContent = label;
    $("statusDot").className = "status-dot " + cls;
  }

  function applyWorkspace(ws, opts = {}) {
    if (!ws) return;
    state.ws = ws;
    if (ws.camera) state.camera = { ...state.camera, ...ws.camera };
    if (ws.clip) state.clip = { ...state.clip, ...ws.clip };
    state.camera.yaw = state.camera.yaw || -2.23;
    state.camera.pitch = Number.isFinite(state.camera.pitch) ? state.camera.pitch : 0.56;
    state.camera.distance = 10;
    state.camera.targetX = 0;
    state.camera.targetY = 0;
    state.camera.targetZ = -0.3;
    state.camera.width = 720;
    state.camera.height = 480;
    $("datasetName").textContent = ws.datasetName || "RIDGE TERRAIN";
    $("seedLabel").textContent = `SEED ${ws.seed || 195}`;
    $("sourceCount").textContent = `${ws.versions?.length || 0} / ${ws.versions?.length || 0}`;
    const active = (ws.versions || []).find((v) => v.id === ws.activeVersion);
    $("activeSourcePill").textContent = (active?.id || "").toUpperCase().replace("/", " ");
    $("versionPill").textContent = `V${ws.version || 1}`;
    $("schemaEncoding").textContent = (ws.source?.encoding || "").toUpperCase();
    $("headerBytes").textContent = fmtBytes(ws.source?.headerMeta?.headerBytes || 0);
    $("recordBytes").textContent = fmtBytes(ws.source?.headerMeta?.recordBytes || 0);
    $("quantization").textContent = ws.source?.quantization?.kind || "—";
    $("sourcePoints").textContent = fmt(ws.stats?.sourcePoints);
    $("pipelinePoints").textContent = fmt(ws.stats?.pipelinePoints);
    $("outlierPoints").textContent = fmt(ws.stats?.outliers);
    $("meshFaces").textContent = fmt(ws.stats?.faces);
    $("telemetryTriangles").textContent = fmt(ws.stats?.faces);
    $("telemetryChunks").textContent = fmt(ws.stats?.chunks);
    $("reusedCount").textContent = `${(ws.chunks || []).filter((c) => c.reused).length} / ${ws.chunks?.length || 12} REUSED`;
    $("algorithmLabel").textContent = (ws.mesh?.algorithm || "LOCAL FIT").replace(/^strata-/, "").split("/")[0].toUpperCase();
    $("stepDenoise").textContent = `${(ws.stats?.previewLatencyMs || 4.2).toFixed(1)} ms`;

    renderSourceCards(ws);
    renderSchema(ws);
    renderParams(ws);
    renderChunks(ws);
    renderExportOptions(ws);
    applyClipInputs();
    renderMetrics(ws);
    draw();
  }

  function renderSourceCards(ws) {
    const box = $("sourceCards");
    box.replaceChildren();
    for (const version of ws.versions || []) {
      const btn = document.createElement("button");
      btn.className = "source-card" + (version.id === ws.activeVersion ? " active" : "");
      btn.innerHTML = `
        <span class="source-badge">${version.short}</span>
        <span>
          <span class="source-name">${version.displayName}</span>
          <span class="source-meta">${version.fieldOrder.join(" · ")}</span>
        </span>
        <span class="source-size">${fmtBytes(version.fileBytes)}</span>`;
      btn.addEventListener("click", async () => {
        if (state.busy) return;
        state.busy = true;
        setStatus("LOADING", "busy");
        notify(`Loading ${version.id}`, "mapping source schema into the canonical model");
        const res = await dispatch("load", { version: version.id });
        state.busy = false;
        if (!res.ok) return showError(res.error);
        applyWorkspace(res.data);
        setStatus("READY");
        notify("Source mapped", `${version.id} · ${res.data.stats.pipelinePoints} pipeline points`, "success");
      });
      box.appendChild(btn);
    }
  }

  function renderSchema(ws) {
    const box = $("fieldChips");
    box.replaceChildren();
    for (const field of ws.source?.fields || []) {
      const chip = document.createElement("span");
      chip.className = "field-chip";
      chip.innerHTML = `<b>${field.index}</b> ${field.name}`;
      chip.title = `${field.path} · ${field.type} · offset ${field.offset}`;
      box.appendChild(chip);
    }
  }

  const paramMap = {
    pVoxelSize: "voxelSize",
    pNormalK: "normalK",
    pOutlierSigma: "outlierSigma",
    pResampleSpacing: "resampleSpacing",
    pChunkSize: "chunkSize",
    pPreviewSample: "previewSample",
    pMaxWorkers: "maxWorkers",
    pVertexBufferLimit: "vertexBufferLimit",
  };

  function renderParams(ws) {
    for (const [id, key] of Object.entries(paramMap)) {
      if (document.activeElement !== $(id)) $(id).value = ws.params?.[key] ?? "";
    }
  }

  function renderChunks(ws) {
    const list = $("chunkList");
    list.replaceChildren();
    for (const chunk of ws.chunks || []) {
      const row = document.createElement("div");
      const reused = chunk.reused;
      row.className = `chunk-row${reused ? " reused" : ""}${chunk.error ? " failed" : ""}`;
      row.innerHTML = `
        <span class="chunk-id">${chunk.id}</span>
        <span><span class="chunk-stat">${chunk.faceCount} tri · ${chunk.vertexCount} vert</span>
        <span class="chunk-reason">${chunk.error || chunk.reason || (reused ? "reused:unchanged" : "rebuilt")}${chunk.cacheHit ? " · sqlite" : ""}</span></span>
        <span class="chunk-dot"></span>`;
      row.title = `${chunk.id} · ${chunk.contentHash || ""}`;
      list.appendChild(row);
    }
  }

  function renderExportOptions(ws) {
    const select = $("exportVersion");
    const previous = select.value;
    select.replaceChildren();
    for (const version of ws.versions || []) {
      const option = document.createElement("option");
      option.value = version.id;
      option.textContent = version.displayName;
      select.appendChild(option);
    }
    if (previous) select.value = previous;
  }

  function renderMetrics(ws) {
    const preview = ws.lastPreview?.latencyMs || ws.stats?.previewLatencyMs || state.drawMs || 0;
    $("previewLatency").textContent = Number(preview).toFixed(2);
    const latencyCard = $("previewLatency").closest(".metric-card");
    latencyCard.classList.toggle("good", preview < 50);
    latencyCard.classList.toggle("bad", preview >= 50);
    updateComparison(ws.lastComparison || {
      coverageDeviation: 0, bboxProjectionDelta: 0, normalHistogramL1: 0,
    });
    $("sampleRateLabel").textContent = Number(ws.lastExact?.sampleRate ?? 1).toFixed(3);
    $("telemetryMode").textContent = state.mode;
  }

  function updateComparison(cmp) {
    if (!cmp) return;
    const cov = (cmp.coverageDeviation || 0) * 100;
    const bbox = (cmp.bboxProjectionDelta || 0) * 100;
    const norm = (cmp.normalHistogramL1 || 0) * 100;
    $("coverageDelta").textContent = cov.toFixed(3);
    $("bboxDelta").textContent = bbox.toFixed(3);
    $("normalDelta").textContent = norm.toFixed(3);
    for (const [id, value, tol] of [
      ["coverageDelta", cov, 2], ["bboxDelta", bbox, 1], ["normalDelta", norm, 18],
    ]) {
      const card = $(id).closest(".metric-card");
      card.classList.toggle("good", value <= tol);
      card.classList.toggle("bad", value > tol);
    }
  }

  function applyClipInputs() {
    $("clipEnabled").checked = !!state.clip.enabled;
    $("clipAxis").value = state.clip.axis || "z";
    $("clipValue").value = state.clip.value || 0;
    $("clipOutput").textContent = Number(state.clip.value || 0).toFixed(3);
    $("clipAbove").checked = state.clip.keepAbove !== false;
  }

  function getClip() {
    return {
      enabled: $("clipEnabled").checked,
      axis: $("clipAxis").value,
      value: Number($("clipValue").value),
      keepAbove: $("clipAbove").checked,
    };
  }

  function currentCamera() {
    return { ...state.camera, width: 720, height: 480 };
  }

  function cameraVectors(cam, viewportWidth, viewportHeight) {
    const cp = Math.cos(cam.pitch);
    const dir = [cp * Math.cos(cam.yaw), cp * Math.sin(cam.yaw), Math.sin(cam.pitch)];
    const target = [cam.targetX, cam.targetY, cam.targetZ];
    const camera = target.map((v, i) => v + dir[i] * cam.distance);
    const norm = (v) => {
      const l = Math.hypot(...v) || 1;
      return v.map((x) => x / l);
    };
    const sub = (a, b) => a.map((v, i) => v - b[i]);
    const cross = (a, b) => [a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0]];
    const view = norm(sub(target, camera));
    const right = norm(cross(view, [0, 0, 1]));
    const up = norm(cross(right, view));
    const focal = 0.5 * viewportHeight / Math.tan((cam.fov * Math.PI) / 360);
    return { camera, view, right, up, focal };
  }

  function projectPoint(p, frame, width, height) {
    const rel = [p[0] - frame.camera[0], p[1] - frame.camera[1], p[2] - frame.camera[2]];
    const z = rel[0] * frame.view[0] + rel[1] * frame.view[1] + rel[2] * frame.view[2];
    if (z <= 0.02) return null;
    const x = rel[0] * frame.right[0] + rel[1] * frame.right[1] + rel[2] * frame.right[2];
    const y = rel[0] * frame.up[0] + rel[1] * frame.up[1] + rel[2] * frame.up[2];
    return {
      x: width * 0.5 + frame.focal * x / z,
      y: height * 0.5 - frame.focal * y / z,
      z,
    };
  }

  function clipWorldPolygon(poly, clip) {
    if (!clip.enabled) return poly;
    const axis = clip.axis === "x" ? 0 : clip.axis === "y" ? 1 : 2;
    const inside = (p) => clip.keepAbove ? p[axis] >= clip.value : p[axis] <= clip.value;
    const out = [];
    let prev = poly[poly.length - 1];
    let prevIn = inside(prev.p);
    for (const cur of poly) {
      const curIn = inside(cur.p);
      if (curIn !== prevIn) {
        const d0 = prev.p[axis] - clip.value;
        const d1 = cur.p[axis] - clip.value;
        const denom = d0 - d1;
        const t = Math.abs(denom) > 1e-12 ? d0 / denom : 0.5;
        out.push({
          p: prev.p.map((v, i) => v + (cur.p[i] - v) * t),
          c: prev.c.map((v, i) => v + (cur.c[i] - v) * t),
        });
      }
      if (curIn) out.push(cur);
      prev = cur;
      prevIn = curIn;
    }
    return out;
  }

  function drawGrid(ctx, w, h, frame) {
    const z = -0.78;
    const lines = [];
    for (let x = -2.4; x <= 2.4001; x += 0.3) {
      lines.push([projectPoint([x, -1.95, z], frame, w, h), projectPoint([x, 2.05, z], frame, w, h)]);
    }
    for (let y = -1.8; y <= 1.8001; y += 0.3) {
      lines.push([projectPoint([-2.55, y, z], frame, w, h), projectPoint([2.55, y, z], frame, w, h)]);
    }
    ctx.save();
    ctx.lineWidth = 1;
    for (let i = 0; i < lines.length; i++) {
      const a = lines[i][0], b = lines[i][1];
      if (!a || !b) continue;
      ctx.strokeStyle = i % 5 === 0 ? "rgba(64,86,88,.30)" : "rgba(48,68,70,.18)";
      ctx.beginPath();
      ctx.moveTo(a.x, a.y);
      ctx.lineTo(b.x, b.y);
      ctx.stroke();
    }
    ctx.restore();
  }

  function draw() {
    const canvas = $("viewport");
    const stage = $("stage");
    const dpr = Math.min(2, window.devicePixelRatio || 1);
    const cssW = stage.clientWidth;
    const cssH = stage.clientHeight;
    if (!cssW || !cssH || !state.ws?.mesh) return;
    if (canvas.width !== Math.round(cssW * dpr) || canvas.height !== Math.round(cssH * dpr)) {
      canvas.width = Math.round(cssW * dpr);
      canvas.height = Math.round(cssH * dpr);
    }
    const ctx = canvas.getContext("2d");
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    const w = cssW, h = cssH;
    const start = performance.now();
    ctx.clearRect(0, 0, w, h);
    ctx.fillStyle = "#05090a";
    ctx.fillRect(0, 0, w, h);
    const grad = ctx.createRadialGradient(w * 0.52, h * 0.48, 0, w * 0.52, h * 0.48, Math.max(w, h) * 0.62);
    grad.addColorStop(0, "rgba(26,48,49,.10)");
    grad.addColorStop(1, "rgba(5,9,10,0)");
    ctx.fillStyle = grad;
    ctx.fillRect(0, 0, w, h);

    const frame = cameraVectors(state.camera, w, h);
    drawGrid(ctx, w, h, frame);
    const mesh = state.ws.mesh;
    const pos = mesh.positions;
    const colors = mesh.colors;
    const idx = mesh.indices;
    const clip = getClip();
    state.clip = clip;
    const light = (() => { const v = [-0.4, -0.55, 0.74]; const l = Math.hypot(...v); return v.map((x) => x / l); })();
    const triangles = [];

    for (let i = 0; i < idx.length; i += 3) {
      const ia = idx[i] * 3, ib = idx[i + 1] * 3, ic = idx[i + 2] * 3;
      let poly = [
        { p: [pos[ia], pos[ia + 1], pos[ia + 2]], c: [colors[ia], colors[ia + 1], colors[ia + 2]] },
        { p: [pos[ib], pos[ib + 1], pos[ib + 2]], c: [colors[ib], colors[ib + 1], colors[ib + 2]] },
        { p: [pos[ic], pos[ic + 1], pos[ic + 2]], c: [colors[ic], colors[ic + 1], colors[ic + 2]] },
      ];
      if (clip.enabled) {
        poly = clipWorldPolygon(poly, clip);
        if (poly.length < 3) continue;
      }
      const projected = poly.map((v) => ({ ...v, s: projectPoint(v.p, frame, w, h) }));
      if (projected.some((v) => !v.s)) continue;
      const depth = projected.reduce((s, v) => s + v.s.z, 0) / projected.length;
      const e1 = [poly[1].p[0] - poly[0].p[0], poly[1].p[1] - poly[0].p[1], poly[1].p[2] - poly[0].p[2]];
      const e2 = [poly[2].p[0] - poly[0].p[0], poly[2].p[1] - poly[0].p[1], poly[2].p[2] - poly[0].p[2]];
      const n = [e1[1] * e2[2] - e1[2] * e2[1], e1[2] * e2[0] - e1[0] * e2[2], e1[0] * e2[1] - e1[1] * e2[0]];
      const nl = Math.hypot(...n) || 1;
      const normal = n.map((v) => v / nl);
      const shade = 0.31 + 0.69 * Math.abs(normal[0] * light[0] + normal[1] * light[1] + normal[2] * light[2]);
      const c = projected.reduce((s, v) => [s[0] + v.c[0], s[1] + v.c[1], s[2] + v.c[2]], [0, 0, 0]).map((v) => v / projected.length);
      triangles.push({ projected, depth, color: c, shade });
    }
    triangles.sort((a, b) => b.depth - a.depth);
    for (const tri of triangles) {
      ctx.beginPath();
      ctx.moveTo(tri.projected[0].s.x, tri.projected[0].s.y);
      for (let k = 1; k < tri.projected.length; k++) ctx.lineTo(tri.projected[k].s.x, tri.projected[k].s.y);
      ctx.closePath();
      const r = Math.max(0, Math.min(255, tri.color[0] * tri.shade));
      const g = Math.max(0, Math.min(255, tri.color[1] * tri.shade));
      const b = Math.max(0, Math.min(255, tri.color[2] * tri.shade));
      ctx.fillStyle = `rgb(${r | 0},${g | 0},${b | 0})`;
      ctx.fill();
      ctx.strokeStyle = ctx.fillStyle;
      ctx.lineWidth = 0.55;
      ctx.stroke();
    }
    state.drawMs = performance.now() - start;
    if (!state.dragging) {
      $("telemetryLatency").textContent = `${state.drawMs.toFixed(2)} ms`;
      $("telemetryLatency").classList.toggle("live", state.drawMs < 50);
    }
    $("sampleRateLabel").textContent = state.mode === "preview"
      ? Number(state.ws.params?.previewSample || 0.35).toFixed(3) : "1.000";
  }

  function stageCamera() {
    return { ...state.camera, width: 720, height: 480 };
  }

  async function doPreview(showToast = false) {
    state.mode = "preview";
    $("modeLabel").textContent = "PREVIEW";
    $("shadingLabel").textContent = "SIMPLIFIED SHADING";
    $("telemetryMode").textContent = "preview";
    setStatus("PREVIEW", "busy");
    const res = await dispatch("preview", { camera: stageCamera(), clip: state.clip });
    if (!res.ok) return showError(res.error);
    const frame = res.data.frame;
    $("previewLatency").textContent = Number(frame.latencyMs).toFixed(2);
    $("telemetryLatency").textContent = `${Number(frame.latencyMs).toFixed(2)} ms`;
    $("telemetryLatency").classList.toggle("live", frame.latencyMs < 50);
    setStatus(frame.withinBudget ? "READY" : "SLOW", frame.withinBudget ? "" : "error");
    if (showToast) notify("Preview frame", `${frame.latencyMs.toFixed(2)} ms · sample ${frame.sampleRate.toFixed(2)} · flat normals`, "success");
    return res.data;
  }

  async function doExact() {
    state.mode = "exact";
    $("modeLabel").textContent = "EXACT";
    $("shadingLabel").textContent = "FULL SHADING";
    $("telemetryMode").textContent = "exact";
    setStatus("SOLVING", "busy");
    const res = await dispatch("exact", { camera: stageCamera(), clip: state.clip });
    if (!res.ok) {
      if (res.error?.code === "E_DIVERGE" && res.data?.comparison) updateComparison(res.data.comparison);
      return showError(res.error);
    }
    updateComparison(res.data.comparison);
    $("previewLatency").textContent = Number(res.data.preview?.latencyMs || 0).toFixed(2);
    $("telemetryLatency").textContent = `${Number(res.data.exact?.latencyMs || 0).toFixed(2)} ms`;
    $("sampleRateLabel").textContent = "1.000";
    setStatus("READY");
    notify("Exact reconstruction", `preview/exact agree · ${res.data.frozenChunks} chunks frozen`, "success");
  }

  function scheduleSettle(delay = 110) {
    window.clearTimeout(state.settleTimer);
    state.settleTimer = window.setTimeout(async () => {
      await doPreview(false);
      await doExact();
    }, delay);
  }

  function bindPointer() {
    const canvas = $("viewport");
    const stage = $("stage");
    canvas.addEventListener("pointerdown", (ev) => {
      canvas.setPointerCapture(ev.pointerId);
      state.dragging = true;
      state.dragMode = ev.shiftKey ? "pan" : "orbit";
      state.lastPointer = { x: ev.clientX, y: ev.clientY };
      stage.classList.add("dragging");
      state.mode = "preview";
      $("modeLabel").textContent = "LIVE PREVIEW";
      $("shadingLabel").textContent = "FLAT NORMALS";
      setStatus("INTERACTING", "busy");
    });
    canvas.addEventListener("pointermove", (ev) => {
      if (!state.dragging || !state.lastPointer) return;
      const dx = ev.clientX - state.lastPointer.x;
      const dy = ev.clientY - state.lastPointer.y;
      state.lastPointer = { x: ev.clientX, y: ev.clientY };
      if (state.dragMode === "pan") {
        const speed = state.camera.distance * 0.0017;
        state.camera.targetX -= dx * speed * Math.cos(state.camera.yaw);
        state.camera.targetY -= dx * speed * Math.sin(state.camera.yaw);
        state.camera.targetZ += dy * speed;
      } else {
        state.camera.yaw -= dx * 0.008;
        state.camera.pitch = Math.max(-1.4, Math.min(1.4, state.camera.pitch + dy * 0.006));
      }
      draw();
      $("telemetryLatency").textContent = `${state.drawMs.toFixed(2)} ms`;
      $("telemetryLatency").classList.toggle("live", state.drawMs < 50);
      const now = performance.now();
      if (now - state.lastApiPreview > 120) {
        state.lastApiPreview = now;
        dispatch("preview", { camera: stageCamera(), clip: state.clip }).then((res) => {
          if (res.ok) {
            const latency = res.data.frame.latencyMs;
            $("previewLatency").textContent = Number(latency).toFixed(2);
            $("telemetryLatency").textContent = `${Number(latency).toFixed(2)} ms`;
          }
        });
      }
    });
    const end = (ev) => {
      if (!state.dragging) return;
      state.dragging = false;
      state.lastPointer = null;
      stage.classList.remove("dragging");
      if (ev.pointerId !== undefined && canvas.hasPointerCapture(ev.pointerId)) canvas.releasePointerCapture(ev.pointerId);
      scheduleSettle(60);
    };
    canvas.addEventListener("pointerup", end);
    canvas.addEventListener("pointercancel", end);
    canvas.addEventListener("wheel", (ev) => {
      ev.preventDefault();
      state.camera.distance = Math.max(2, Math.min(20, state.camera.distance * Math.exp(ev.deltaY * 0.0011)));
      draw();
      scheduleSettle(170);
    }, { passive: false });
  }

  function collectParams() {
    const params = {};
    for (const [id, key] of Object.entries(paramMap)) params[key] = Number($(id).value);
    params.chunkBias = state.ws?.params?.chunkBias || {};
    return params;
  }

  async function applyAction(action, payload, successTitle) {
    if (state.busy) return;
    state.busy = true;
    setStatus("WORKING", "busy");
    const res = await dispatch(action, payload);
    state.busy = false;
    if (!res.ok) {
      setStatus("READY");
      showError(res.error);
      return;
    }
    if (res.data?.workspace) applyWorkspace(res.data.workspace, { keepCamera: true });
    setStatus("READY");
    if (successTitle) notify(successTitle, "", "success");
    return res.data;
  }

  function bindControls() {
    document.querySelectorAll(".tab").forEach((tab) => {
      tab.addEventListener("click", () => {
        document.querySelectorAll(".tab").forEach((t) => t.classList.toggle("active", t === tab));
        $("paramsPanel").classList.toggle("hidden", tab.dataset.tab !== "params");
        $("chunksPanel").classList.toggle("hidden", tab.dataset.tab !== "chunks");
      });
    });

    $("clipEnabled").addEventListener("change", () => { state.clip = getClip(); draw(); scheduleSettle(80); });
    $("clipAxis").addEventListener("change", () => { state.clip = getClip(); draw(); scheduleSettle(80); });
    $("clipValue").addEventListener("input", () => {
      state.clip = getClip();
      $("clipOutput").textContent = state.clip.value.toFixed(3);
      draw();
    });
    $("clipValue").addEventListener("change", () => scheduleSettle(80));
    $("clipAbove").addEventListener("change", () => { state.clip = getClip(); draw(); scheduleSettle(80); });

    $("previewBtn").addEventListener("click", async () => { await doPreview(true); });
    $("exactBtn").addEventListener("click", async () => { await doExact(); draw(); });
    $("snapshotBtn").addEventListener("click", async () => {
      const data = await applyAction("update_params", { params: collectParams(), camera: stageCamera(), clip: state.clip }, "Parameter snapshot committed");
      if (data?.incremental) notify("Invalidation plan", `${data.incremental.manifest.rebuilt} rebuilt · ${data.incremental.manifest.reused} reused`, "warn");
    });
    $("editBtn").addEventListener("click", async () => {
      const data = await applyAction("edit_point", {
        index: Number($("editIndex").value),
        deltaZ: Number($("editDelta").value),
        baseVersion: state.ws?.version,
        camera: stageCamera(),
        clip: state.clip,
      }, "Point edit committed");
      if (data?.incremental) notify("Local invalidation", `${data.incremental.manifest.rebuilt} rebuilt · ${data.incremental.manifest.reused} reused · ${data.edit?.chunkId || ""}`, "warn");
    });
    $("incrementalBtn").addEventListener("click", async () => {
      const data = await applyAction("incremental", {
        index: Number($("editIndex").value || 0),
        deltaZ: Number($("editDelta").value || 0.02),
        camera: stageCamera(),
        clip: state.clip,
      }, "Incremental rebuild complete");
      if (data?.incremental) {
        notify("Incremental manifest", `${data.incremental.manifest.rebuilt} rebuilt · ${data.incremental.manifest.reused} reused · ${data.incremental.manifest.cacheHits} sqlite hits`, "warn");
        document.querySelector('[data-tab="chunks"]').click();
      }
    });
    $("pauseBtn").addEventListener("click", async () => {
      const res = await dispatch("start_job", { camera: stageCamera(), clip: state.clip });
      if (!res.ok) return showError(res.error);
      setStatus("BUILDING", "busy");
      window.setTimeout(async () => {
        const paused = await dispatch("pause", {});
        if (paused.ok) {
          notify("Checkpoint paused", `${paused.data.job.committed}/${paused.data.job.total} chunks committed`, "warn");
          setStatus("PAUSED", "busy");
        } else showError(paused.error);
      }, 12);
    });
    $("resumeBtn").addEventListener("click", async () => {
      const res = await dispatch("resume", {});
      if (!res.ok) return showError(res.error);
      notify("Resuming from SQLite", "committed chunks are skipped", "success");
      setStatus("BUILDING", "busy");
      const timer = window.setInterval(async () => {
        const jobs = await dispatch("jobs", {});
        const job = jobs.data?.jobs?.[0];
        if (job && (job.status === "complete" || job.status === "failed" || job.status === "paused")) {
          window.clearInterval(timer);
          setStatus(job.status === "failed" ? "ERROR" : "READY", job.status === "failed" ? "error" : "");
          notify("Exact job", `${job.status} · ${job.committed}/${job.total}`, job.status === "failed" ? "error" : "success");
        }
      }, 450);
    });
    $("verifyBtn").addEventListener("click", async () => {
      const data = await applyAction("verify_paths", {}, null);
      if (!data) return;
      if (data.passed) notify("4-path verification passed", `${data.fullMeshHash.slice(0, 12)}… · identical checkpoint sequence`, "success", 14000);
      else notify("E_DIFF", `first differing chunk ${data.firstDifferingChunk}`, "error", 14000);
    });
    $("exportBtn").addEventListener("click", async () => {
      const res = await dispatch("export", {
        versionId: $("exportVersion").value,
        kind: $("exportMesh").checked ? "mesh" : "cloud",
      });
      if (!res.ok) return showError(res.error);
      const d = res.data;
      const warningText = d.warnings?.length ? `${d.warnings.length} × E_LOSSY` : "lossless round-trip";
      notify("Export verified", `${d.readbackPoints} pts · ${d.readbackFaces} faces · ${warningText}`, d.warnings?.length ? "warn" : "success", 14000);
      for (const warning of (d.warnings || []).slice(0, 2)) notify(warning.code, warning.message, "warn", 14000);
      if (location.protocol.startsWith("http")) {
        const a = document.createElement("a");
        a.href = `/api/download?path=${encodeURIComponent(d.path)}`;
        a.download = d.path.split(/[\\/]/).pop();
        a.click();
      }
    });

    $("openCloudBtn").addEventListener("click", () => {
      if (window.go?.main?.DesktopApp) {
        notify("Local loader", "Use the native Windows build with the bundled local samples; browser mode supports file upload.", "warn");
        return;
      }
      $("fileInput").click();
    });
    $("fileInput").addEventListener("change", async (ev) => {
      const file = ev.target.files?.[0];
      if (!file) return;
      const form = new FormData();
      form.append("file", file);
      setStatus("LOADING", "busy");
      notify("Uploading local cloud", file.name);
      try {
        const res = await fetch("/api/upload", { method: "POST", body: form });
        const data = await res.json();
        if (!data.ok) return showError(data.error);
        applyWorkspace(data.data);
        setStatus("READY");
        notify("Local point cloud loaded", `${data.data.source.file}`, "success");
      } catch (err) {
        showError({ code: "E_DECODE", message: String(err) });
      }
    });
    window.addEventListener("keydown", (ev) => {
      if (ev.key === "Escape") {
        state.dragging = false;
        state.lastPointer = null;
      }
    });
  }

  async function boot() {
    window.__strata = state;
    bindPointer();
    bindControls();
    const res = await dispatch("bootstrap", {});
    if (!res.ok) return showError(res.error);
    applyWorkspace(res.data);
    notify("Loaded source cloud", `${res.data.source.displayName} · ${fmt(res.data.stats.sourcePoints)} points`, "success");
    notify("Pipeline ready", `${res.data.stats.chunks} spatial chunks · ${fmt(res.data.stats.faces)} faces`, "success");
    const ro = new ResizeObserver(() => draw());
    ro.observe($("stage"));
    window.setTimeout(async () => {
      await doPreview(false);
      await doExact();
    }, 120);
  }

  boot();
})();
