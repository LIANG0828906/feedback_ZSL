(() => {
  const $ = (id) => document.getElementById(id);
  const state = {
    bootstrap: null,
    patches: [],
    frame: null,
    image: null,
    exact: null,
    preview: null,
    reference: null,
    invalidation: null,
    comparison: null,
    queue: null,
    profile: null,
    selectedTile: 0,
    failedTiles: new Set(),
    busy: false,
  };

  class ApiError extends Error {
    constructor(diag) {
      super((diag && diag.message) || "API failure");
      this.diag = diag || { code: "E_STAGE", message: this.message };
    }
  }

  async function apiCall(action, payload = {}) {
    const body = JSON.stringify(payload || {});
    let response;
    const bridge = window.go && (
      (window.go.app && window.go.app.App && window.go.app.App.Call) ||
      (window.go.main && window.go.main.App && window.go.main.App.Call)
    );
    if (typeof bridge === "function") {
      response = await bridge(action, body);
    } else {
      const r = await fetch("/api/call", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action, payload }),
      });
      if (!r.ok) {
        throw new ApiError({ code: "E_STAGE", message: `HTTP ${r.status}` });
      }
      response = await r.json();
    }
    if (!response || typeof response !== "object") {
      throw new ApiError({ code: "E_STAGE", message: "bridge returned no response" });
    }
    if (!response.ok) {
      throw new ApiError(response.error);
    }
    return response.data;
  }

  function fmtDiag(d) {
    if (!d) return "unknown diagnostic";
    const bits = [d.code];
    if (Number.isInteger(d.patchId) && d.patchId >= 0) bits.push(`patch ${d.patchId}`);
    if (Number.isInteger(d.tileId) && d.tileId >= 0) bits.push(`tile ${d.tileId}`);
    if (d.field) bits.push(d.field);
    if (d.message) bits.push(d.message);
    if (d.versionA || d.versionB) bits.push(`v${d.versionA || "?"}↔v${d.versionB || "?"}`);
    if (d.mislabeledTiles && d.mislabeledTiles.length) bits.push(`mislabeled [${d.mislabeledTiles.join(",")}]`);
    if (d.conflictingTiles && d.conflictingTiles.length) bits.push(`conflicts [${d.conflictingTiles.join(",")}]`);
    if (d.firstPixel) bits.push(`@(${d.firstPixel.x},${d.firstPixel.y})`);
    return bits.join(" · ");
  }

  function log(text, level = "") {
    const node = $("eventLog");
    const row = document.createElement("div");
    row.className = `log-entry ${level}`;
    const now = new Date();
    const time = `${String(now.getHours()).padStart(2, "0")}:${String(now.getMinutes()).padStart(2, "0")}:${String(now.getSeconds()).padStart(2, "0")}`;
    row.innerHTML = `<time>${time}</time><span></span>`;
    row.querySelector("span").textContent = text;
    node.prepend(row);
    while (node.children.length > 80) node.lastChild.remove();
  }

  let toastTimer = 0;
  function toast(text, isError = false) {
    const node = $("toast");
    node.textContent = text;
    node.className = `toast show${isError ? " err" : ""}`;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { node.className = "toast"; }, 3800);
  }

  async function busy(label, fn) {
    if (state.busy) return null;
    state.busy = true;
    $("busyText").textContent = label;
    $("busyOverlay").classList.add("show");
    try {
      return await fn();
    } catch (err) {
      const d = err instanceof ApiError ? err.diag : { code: "E_STAGE", message: String(err) };
      $("renderLed").className = "led err";
      log(fmtDiag(d), "err");
      toast(fmtDiag(d), true);
      return null;
    } finally {
      state.busy = false;
      $("busyOverlay").classList.remove("show");
    }
  }

  function hashShort(hash) {
    return hash ? hash.slice(0, 12) + "…" : "—";
  }

  function setSnapshotMeta(frame) {
    if (!frame) return;
    $("snapshotVersion").textContent = `v${frame.version}`;
    $("snapshotHash").textContent = hashShort(frame.contentHash);
    $("pathValue").textContent = frame.path.toUpperCase();
    $("lodValue").textContent = `LOD ${frame.lod}`;
    $("durationValue").textContent = `${(frame.durationNs / 1e6).toFixed(1)} ms`;
    $("cacheValue").textContent = `${frame.cacheHits} hit / ${frame.rendered} calc`;
    $("framePathCorner").textContent = `${frame.path.toUpperCase()} / LOD ${frame.lod}`;
    $("frameVersionCorner").textContent = `SNAPSHOT v${frame.version}`;
    $("cameraValue").textContent = `eye ${vec(frame.camera.eye)} / fov ${frame.camera.fovY}°`;
    $("clipValue").textContent = `${vec(frame.clipPlane.normal)} + ${frame.clipPlane.offset.toFixed(2)}`;
    $("algorithmValue").textContent = frame.algorithmVersion;
    $("stageSubtitle").textContent = `${frame.grid.cols * frame.grid.rows} tiles / ${frame.width}×${frame.height}`;
    $("gridTag").textContent = `${frame.grid.cols} × ${frame.grid.rows}`;
  }

  function vec(v) {
    if (!v) return "—";
    return `(${Number(v.x).toFixed(2)}, ${Number(v.y).toFixed(2)}, ${Number(v.z).toFixed(2)})`;
  }

  function setFrame(frame) {
    if (!frame) return;
    state.frame = frame;
    setSnapshotMeta(frame);
    $("emptyState").classList.add("hidden");
    $("renderLed").className = "led ok";
    if (frame.queue) state.queue = frame.queue;
    state.image = null;
    const img = new Image();
    img.onload = () => {
      if (state.frame !== frame) return;
      state.image = img;
      fitCanvas();
      draw();
    };
    img.src = `data:image/png;base64,${frame.pngBase64}`;
    renderTileMatrix();
    updateQueue();
    updateTileInspector();
  }

  function fitCanvas() {
    const canvas = $("viewport");
    const wrap = $("viewportWrap");
    if (!state.frame) return;
    const rect = wrap.getBoundingClientRect();
    const aspect = state.frame.width / state.frame.height;
    let w = rect.width;
    let h = w / aspect;
    if (h > rect.height) {
      h = rect.height;
      w = h * aspect;
    }
    canvas.width = state.frame.width;
    canvas.height = state.frame.height;
    canvas.style.position = "absolute";
    canvas.style.width = `${Math.max(1, w)}px`;
    canvas.style.height = `${Math.max(1, h)}px`;
    canvas.style.left = `${Math.max(0, (rect.width - w) / 2)}px`;
    canvas.style.top = `${Math.max(0, (rect.height - h) / 2)}px`;
    canvas.style.objectFit = "fill";
  }

  function draw() {
    const canvas = $("viewport");
    if (!state.frame) return;
    const ctx = canvas.getContext("2d");
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    if (state.image) ctx.drawImage(state.image, 0, 0, canvas.width, canvas.height);
    drawTileOverlay(ctx);
    drawProfile(ctx);
  }

  function drawTileOverlay(ctx) {
    const f = state.frame;
    const g = f.grid;
    ctx.save();
    ctx.lineWidth = 1;
    ctx.strokeStyle = "rgba(109, 157, 169, .18)";
    for (let c = 1; c < g.cols; c++) {
      const x = Math.round(g.width * c / g.cols) + 0.5;
      ctx.beginPath(); ctx.moveTo(x, 0); ctx.lineTo(x, g.height); ctx.stroke();
    }
    for (let r = 1; r < g.rows; r++) {
      const y = Math.round(g.height * r / g.rows) + 0.5;
      ctx.beginPath(); ctx.moveTo(0, y); ctx.lineTo(g.width, y); ctx.stroke();
    }
    const invalidated = new Set(state.invalidation ? state.invalidation.invalidatedTiles : []);
    if (invalidated.size) {
      ctx.strokeStyle = "rgba(225, 160, 74, .7)";
      ctx.lineWidth = 1.5;
      for (const id of invalidated) {
        const [x, y, w, h] = tileRect(g, id);
        ctx.strokeRect(x + 1, y + 1, Math.max(0, w - 2), Math.max(0, h - 2));
      }
    }
    if (Number.isInteger(state.selectedTile)) {
      const [x, y, w, h] = tileRect(g, state.selectedTile);
      ctx.strokeStyle = "rgba(83, 211, 194, .95)";
      ctx.lineWidth = 2;
      ctx.strokeRect(x + 1, y + 1, Math.max(0, w - 2), Math.max(0, h - 2));
    }
    ctx.restore();
  }

  function drawProfile(ctx) {
    if (!state.profile) return;
    ctx.save();
    ctx.strokeStyle = "rgba(226, 100, 90, .95)";
    ctx.fillStyle = "rgba(255, 196, 120, .95)";
    ctx.lineWidth = 1.2;
    for (const seg of state.profile.segments) {
      if (!seg.a.visible || !seg.b.visible) continue;
      ctx.beginPath();
      ctx.moveTo(seg.a.screen.u, seg.a.screen.v);
      ctx.lineTo(seg.b.screen.u, seg.b.screen.v);
      ctx.stroke();
      ctx.fillRect(seg.a.screen.u - 1.5, seg.a.screen.v - 1.5, 3, 3);
      ctx.fillRect(seg.b.screen.u - 1.5, seg.b.screen.v - 1.5, 3, 3);
    }
    ctx.restore();
  }

  function tileRect(grid, id) {
    const col = id % grid.cols;
    const row = Math.floor(id / grid.cols);
    const x0 = Math.round(grid.width * col / grid.cols);
    const x1 = Math.round(grid.width * (col + 1) / grid.cols);
    const y0 = Math.round(grid.height * row / grid.rows);
    const y1 = Math.round(grid.height * (row + 1) / grid.rows);
    return [x0, y0, x1 - x0, y1 - y0];
  }

  function renderTileMatrix() {
    const node = $("tileMatrix");
    if (!state.frame) {
      node.innerHTML = '<div class="placeholder">等待渲染帧</div>';
      return;
    }
    const grid = state.frame.grid;
    const invalidated = new Set(state.invalidation ? state.invalidation.invalidatedTiles : []);
    const hasInvalidation = invalidated.size > 0;
    node.style.gridTemplateColumns = `repeat(${grid.cols}, 1fr)`;
    node.innerHTML = "";
    const tileMap = new Map((state.frame.tiles || []).map(t => [t.id, t]));
    for (let id = 0; id < grid.cols * grid.rows; id++) {
      const tile = tileMap.get(id);
      const btn = document.createElement("button");
      btn.className = "tile-cell";
      if (tile && tile.stats.surfacePixels > 0) btn.classList.add("rendered");
      if (hasInvalidation && invalidated.has(id)) btn.classList.add("invalidated");
      else if (hasInvalidation && !invalidated.has(id)) btn.classList.add("cached");
      if (state.failedTiles.has(id)) btn.classList.add("failed");
      if (id === state.selectedTile) btn.classList.add("selected");
      const coverage = tile ? (tile.stats.coverage * 100) : 0;
      btn.title = `tile ${id} · coverage ${coverage.toFixed(1)}% · ${tile ? tile.stats.surfacePixels + " px" : "no data"}`;
      btn.innerHTML = `<span>${id}</span><i class="bar" style="width:${Math.max(0, Math.min(100, coverage))}%"></i>`;
      btn.addEventListener("click", () => {
        state.selectedTile = id;
        renderTileMatrix();
        updateTileInspector();
        draw();
      });
      node.appendChild(btn);
    }
    $("invalidCount").textContent = String(invalidated.size);
  }

  function updateTileInspector() {
    if (!state.frame) return;
    const tile = (state.frame.tiles || []).find(t => t.id === state.selectedTile);
    $("selectedTileLabel").textContent = `TILE ${state.selectedTile}`;
    $("tileSelected").textContent = `TILE ${state.selectedTile}`;
    if (!tile) {
      $("tileCoverage").textContent = "—";
      $("tileDepth").textContent = "—";
      $("tileLum").textContent = "—";
      return;
    }
    $("tileCoverage").textContent = `${(tile.stats.coverage * 100).toFixed(1)}%`;
    $("tileDepth").textContent = `${tile.stats.minDepth.toFixed(3)}–${tile.stats.maxDepth.toFixed(3)}`;
    const peak = Math.max(...tile.stats.luminanceBins);
    const peakBin = tile.stats.luminanceBins.indexOf(peak);
    $("tileLum").textContent = `${peakBin}/15 · ${peak} px`;
  }

  function updateInvalidation(report) {
    state.invalidation = report;
    const node = $("invalidList");
    if (!report || !report.items || !report.items.length) {
      node.innerHTML = '<div class="placeholder">尚无编辑传播记录</div>';
      $("flowSource").textContent = "CONTROL / MATERIAL";
      $("flowPatch").textContent = "PATCH";
      $("flowTile").textContent = "TILES";
      $("flowShade").textContent = "SHADE";
      renderTileMatrix();
      draw();
      return;
    }
    $("flowSource").textContent = String(report.edit.kind).toUpperCase();
    $("flowPatch").textContent = `P${report.edit.patchId}`;
    $("flowTile").textContent = `${report.invalidatedTiles.length} TILE`;
    $("flowShade").textContent = `${report.downstreamStages.length} SHADE`;
    $("invalidCount").textContent = String(report.invalidatedTiles.length);
    node.innerHTML = "";
    for (const item of report.items) {
      const row = document.createElement("div");
      row.className = "invalid-item";
      row.innerHTML = `<div class="head"><span>TILE ${item.tileId} · P${item.patchIds.join(",")}</span><span>${item.stages.join(" / ")}</span></div><p></p>`;
      row.querySelector("p").textContent = item.reasons.join(" · ");
      node.appendChild(row);
    }
    renderTileMatrix();
    draw();
  }

  function updateQueue() {
    const q = state.queue || (state.frame && state.frame.queue) || {};
    const queue = q.queue || [];
    $("parallelMetric").textContent = String(q.peakParallel || 0);
    $("queueMetric").textContent = String(queue.length);
    $("budgetPeak").textContent = `PIX ${fmtBytes(q.peakPixelBytes || 0)} / MESH ${fmtBytes(q.peakMeshBytes || 0)}`;
    const node = $("queueList");
    node.innerHTML = "";
    if (!queue.length) {
      node.innerHTML = '<div class="placeholder">等待精确路径调度</div>';
      return;
    }
    for (const item of queue.slice(0, 8)) {
      const row = document.createElement("div");
      row.className = "queue-row";
      row.innerHTML = `<span>TASK ${item.taskId} · ${item.reason}</span><span>${(item.waitNs / 1e6).toFixed(1)} ms</span>`;
      node.appendChild(row);
    }
  }

  function fmtBytes(n) {
    if (n > 1 << 20) return `${(n / (1 << 20)).toFixed(1)}MB`;
    if (n > 1 << 10) return `${(n / (1 << 10)).toFixed(1)}KB`;
    return `${n}B`;
  }

  function updateComparison(cmp) {
    state.comparison = cmp;
    const status = $("compareStatus");
    if (!cmp) {
      status.textContent = "UNVERIFIED";
      status.className = "status-pill idle";
      $("cmpInc").textContent = "—";
      $("cmpResume").textContent = "—";
      $("cmpCoverage").textContent = "—";
      $("cmpShading").textContent = "—";
      $("diffBox").textContent = "尚未发现像素差异";
      $("diffBox").className = "diff-box";
      return;
    }
    const inc = cmp.fullVsIncremental;
    const resume = cmp.fullVsResume;
    const allOK = !!(inc && inc.equal && (!resume || resume.equal));
    status.textContent = allOK ? "IDENTICAL" : "MISMATCH";
    status.className = `status-pill ${allOK ? "ok" : "err"}`;
    $("cmpInc").textContent = inc ? (inc.equal ? "PIXEL EQUAL" : "DIFF") : "N/A";
    $("cmpResume").textContent = resume ? (resume.equal ? "PIXEL EQUAL" : "DIFF") : "NOT RUN";
    $("cmpCoverage").textContent = cmp.coverageEqual ? "EQUAL" : "DIFF";
    $("cmpShading").textContent = cmp.shadingEqual ? "EQUAL" : "DIFF";
    const d = inc && inc.diagnostic;
    if (d) {
      $("diffBox").className = "diff-box err";
      $("diffBox").textContent = `${fmtDiag(d)}${cmp.firstDiffPixel ? ` · first pixel (${cmp.firstDiffPixel.x}, ${cmp.firstDiffPixel.y}) · tile ${cmp.firstDiffTile} · field ${cmp.firstDiffField}` : ""}`;
    } else {
      $("diffBox").className = "diff-box";
      $("diffBox").textContent = "逐像素、分块轮廓覆盖率与明暗直方图一致";
    }
  }

  function updateCacheMetrics(frame) {
    if (!frame) return;
    $("hitsMetric").textContent = String(frame.cacheHits || 0);
    $("renderedMetric").textContent = String(frame.renderedTiles || 0);
    $("cacheValue").textContent = `${frame.cacheHits || 0} hit / ${frame.renderedTiles || 0} calc`;
  }

  function updateProfile(profile) {
    state.profile = profile;
    $("profileVersion").textContent = `v${profile.version} // LOCKED`;
    $("segmentCount").textContent = `${profile.segments.length} segments`;
    if (!profile.segments.length) {
      $("endpointA").querySelector("span").textContent = "world — / uv —";
      $("endpointB").querySelector("span").textContent = "world — / uv —";
      draw();
      return;
    }
    const seg = profile.segments[0];
    $("endpointA").querySelector("span").textContent = `world ${vec(seg.a.world)} / uv ${fmtUV(seg.a.param)}`;
    $("endpointB").querySelector("span").textContent = `world ${vec(seg.b.world)} / uv ${fmtUV(seg.b.param)}`;
    draw();
  }

  function fmtUV(uv) {
    return `(${Number(uv.u).toFixed(4)}, ${Number(uv.v).toFixed(4)})`;
  }

  function renderPatchOptions() {
    const select = $("patchSelect");
    select.innerHTML = "";
    for (const p of state.patches) {
      const opt = document.createElement("option");
      opt.value = String(p.id);
      opt.textContent = `${p.name} · P${p.id}`;
      select.appendChild(opt);
    }
    select.addEventListener("change", () => populatePatch(Number(select.value)));
    populatePatch(state.patches[0] ? state.patches[0].id : 1);
  }

  function selectedPatch() {
    const id = Number($("patchSelect").value);
    return state.patches.find(p => p.id === id) || state.patches[0];
  }

  function populatePatch(patchID) {
    const patch = state.patches.find(p => p.id === patchID);
    if (!patch) return;
    $("patchBadge").textContent = `P${patch.id}`;
    const cpSelect = $("cpSelect");
    cpSelect.innerHTML = "";
    patch.controlPoints.forEach((cp, idx) => {
      const opt = document.createElement("option");
      opt.value = String(idx);
      opt.textContent = `[${cp.row},${cp.col}] · w ${cp.weight.toFixed(3)}`;
      cpSelect.appendChild(opt);
    });
    cpSelect.onchange = () => populateControl();
    populateControl();
    $("densityRange").value = String(patch.density);
    $("densityValue").textContent = String(patch.density);
    $("matColor").value = rgbHex(patch.material.baseColor);
    $("specRange").value = String(patch.material.specular);
    $("specValue").textContent = Number(patch.material.specular).toFixed(2);
    $("shineRange").value = String(patch.material.shininess);
    $("shineValue").textContent = String(patch.material.shininess);
  }

  function populateControl() {
    const patch = selectedPatch();
    const cp = patch.controlPoints[Number($("cpSelect").value)] || patch.controlPoints[0];
    if (!cp) return;
    $("cpX").value = Number(cp.position.x).toFixed(3);
    $("cpY").value = Number(cp.position.y).toFixed(3);
    $("cpZ").value = Number(cp.position.z).toFixed(3);
  }

  function rgbHex(c) {
    const h = (n) => Math.max(0, Math.min(255, n)).toString(16).padStart(2, "0");
    return `#${h(c.r)}${h(c.g)}${h(c.b)}`;
  }

  function hexRgb(hex) {
    return {
      r: parseInt(hex.slice(1, 3), 16),
      g: parseInt(hex.slice(3, 5), 16),
      b: parseInt(hex.slice(5, 7), 16),
    };
  }

  function applyEditData(data) {
    if (data.frame) setFrame(data.frame);
    state.reference = data.reference;
    updateInvalidation(data.invalidation);
    updateComparison(data.comparison);
    state.queue = data.queue || (data.frame && data.frame.queue);
    updateQueue();
    updateCacheMetrics(data.frame);
    log(`incremental ok · ${data.invalidation.invalidatedTiles.length} invalid / ${data.invalidation.reusedTiles.length} reused · v${data.frame.version}`, "ok");
  }

  async function bootstrap() {
    const data = await apiCall("bootstrap");
    state.bootstrap = data;
    state.patches = data.patches || [];
    $("seedValue").textContent = String(data.seed);
    $("snapshotVersion").textContent = `v${data.state.version}`;
    $("snapshotHash").textContent = hashShort(data.state.contentHash);
    $("gridTag").textContent = `${data.state.tiles.cols} × ${data.state.tiles.rows}`;
    renderPatchOptions();
    updateComparison(null);
    if (data.frame) {
      setFrame(data.frame);
    } else {
      renderTileMatrix();
    }
    log(`bootstrap · seed ${data.seed} · ${data.patches.length} patches · v${data.state.version}`, "ok");
    await busy("PREVIEW FRAME", async () => {
      const result = await apiCall("preview");
      state.preview = result.frame;
      setFrame(result.frame);
      showSnapshotCheck(result.snapshotCheck);
      log(`preview frame · LOD ${result.frame.lod} · ${(result.frame.durationNs / 1e6).toFixed(1)}ms`, "ok");
    });
    await busy("EXACT WARMUP", async () => {
      const result = await apiCall("exact");
      state.exact = result.frame;
      state.reference = result.frame;
      setFrame(result.frame);
      showSnapshotCheck(result.snapshotCheck);
      updateQueue();
      updateCacheMetrics(result.frame);
      log(`exact warmup · ${result.frame.renderedTiles} tiles · ${(result.frame.durationNs / 1e6).toFixed(1)}ms`, "ok");
    });
  }

  function showSnapshotCheck(diag) {
    const node = $("snapshotCheck");
    if (!diag) {
      node.textContent = "PATH SNAPSHOT OK · camera / clip / version bound";
      node.className = "verify-line ok";
    } else {
      node.textContent = fmtDiag(diag);
      node.className = "verify-line err";
    }
  }

  function bindUI() {
    $("btnPreview").addEventListener("click", () => busy("PREVIEW FRAME", async () => {
      const result = await apiCall("preview");
      state.preview = result.frame;
      setFrame(result.frame);
      showSnapshotCheck(result.snapshotCheck);
      log(`preview · LOD ${result.frame.lod} · ${(result.frame.durationNs / 1e6).toFixed(1)}ms`, "ok");
    }));

    $("btnExact").addEventListener("click", () => busy("EXACT TILED RENDER", async () => {
      const result = await apiCall("exact");
      state.exact = result.frame;
      state.reference = result.frame;
      state.invalidation = null;
      state.failedTiles.clear();
      setFrame(result.frame);
      updateInvalidation(null);
      showSnapshotCheck(result.snapshotCheck);
      updateCacheMetrics(result.frame);
      updateQueue();
      log(`exact · ${result.frame.renderedTiles} tiles · ${(result.frame.durationNs / 1e6).toFixed(1)}ms`, "ok");
    }));

    $("btnApplyCP").addEventListener("click", () => {
      const patch = selectedPatch();
      const cp = patch.controlPoints[Number($("cpSelect").value)] || patch.controlPoints[0];
      const payload = {
        patchId: patch.id,
        row: cp.row,
        col: cp.col,
        position: { x: Number($("cpX").value), y: Number($("cpY").value), z: Number($("cpZ").value) },
      };
      busy("CONTROL POINT INCREMENTAL", async () => {
        const data = await apiCall("applyControlPoint", payload);
        applyEditData(data);
        cp.position = { ...payload.position };
      });
    });

    $("btnApplyDensity").addEventListener("click", () => {
      const patch = selectedPatch();
      busy("TESSELLATION INCREMENTAL", async () => {
        const data = await apiCall("applyDensity", { patchId: patch.id, density: Number($("densityRange").value) });
        applyEditData(data);
        patch.density = Number($("densityRange").value);
      });
    });

    $("densityRange").addEventListener("input", (e) => { $("densityValue").textContent = e.target.value; });
    $("specRange").addEventListener("input", (e) => { $("specValue").textContent = Number(e.target.value).toFixed(2); });
    $("shineRange").addEventListener("input", (e) => { $("shineValue").textContent = e.target.value; });
    $("profileOffset").addEventListener("input", (e) => { $("profileOffsetValue").textContent = Number(e.target.value).toFixed(2); });

    $("btnApplyMaterial").addEventListener("click", () => {
      const patch = selectedPatch();
      const material = {
        ...patch.material,
        baseColor: hexRgb($("matColor").value),
        specular: Number($("specRange").value),
        shininess: Number($("shineRange").value),
      };
      busy("MATERIAL INCREMENTAL", async () => {
        const data = await apiCall("applyMaterial", { patchId: patch.id, material });
        applyEditData(data);
        patch.material = material;
      });
    });

    $("btnProfile").addEventListener("click", () => busy("SECTION PROFILE", async () => {
      const result = await apiCall("profile", {
        normal: { x: 0, y: 1, z: 0 },
        offset: Number($("profileOffset").value),
      });
      updateProfile(result);
      log(`profile frozen @ v${result.version} · ${result.segments.length} segments`, "ok");
    }));

    document.querySelectorAll(".fault").forEach(btn => {
      btn.addEventListener("click", () => busy("FAULT INJECTION", async () => {
        const result = await apiCall("anomaly", {
          kind: btn.dataset.kind,
          tileId: state.selectedTile,
        });
        const d = result.diagnostic;
        if (result.kind === "tile-stage-failure") {
          state.failedTiles.add(state.selectedTile);
          renderTileMatrix();
        }
        $("renderLed").className = "led err";
        log(`${d.code} · ${fmtDiag(d)}`, "err");
        toast(fmtDiag(d), true);
      }));
    });

    $("btnRetryTile").addEventListener("click", () => busy("RETRY TILE", async () => {
      const data = await apiCall("retry", { tileId: state.selectedTile });
      state.failedTiles.delete(state.selectedTile);
      setFrame(data.frame);
      updateCacheMetrics(data.frame);
      log(`tile ${state.selectedTile} retried and checkpointed`, "ok");
      toast(`TILE ${state.selectedTile} retry committed`);
    }));

    $("btnCompare").addEventListener("click", () => busy("THREE-PATH COMPARE", async () => {
      const cmp = await apiCall("compare");
      updateComparison(cmp);
      log(cmp.fullVsIncremental && cmp.fullVsIncremental.equal ? "three-path comparison identical" : "three-path comparison mismatch", cmp.fullVsIncremental && cmp.fullVsIncremental.equal ? "ok" : "err");
    }));

    $("btnResume").addEventListener("click", () => busy("CHECKPOINT RESUME", async () => {
      const result = await apiCall("resume");
      if (result.frame) setFrame(result.frame);
      if (result.diagnostic) {
        const d = result.diagnostic;
        $("renderLed").className = "led err";
        log(`${d.code} · ${fmtDiag(d)} · full rerender=${result.fullRerendered}`, "err");
        toast(fmtDiag(d), true);
      } else {
        log(`resume ok · ${result.loadedTiles} committed tiles`, "ok");
        toast(`恢复 ${result.loadedTiles} 个版本匹配分块`);
      }
    }));

    $("btnExport").addEventListener("click", () => busy("EXPORT DIAGNOSTICS", async () => {
      const data = await apiCall("export");
      const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
      const a = document.createElement("a");
      const version = state.frame ? state.frame.version : "state";
      a.href = URL.createObjectURL(blob);
      a.download = `cadlab-diagnostics-v${version}.json`;
      a.click();
      setTimeout(() => URL.revokeObjectURL(a.href), 1000);
      log(`diagnostic export v${version}`, "ok");
    }));

    $("btnClearLog").addEventListener("click", () => { $("eventLog").innerHTML = ""; });
    $("btnCancelBusy").addEventListener("click", async (event) => {
      event.stopPropagation();
      try {
        const result = await apiCall("cancel");
        toast(result.cancelled ? `正在取消 ${result.active}` : "当前没有可取消的渲染", !result.cancelled);
        log(`cancel requested · ${result.active || "idle"}`, result.cancelled ? "err" : "");
      } catch (err) {
        toast(err instanceof ApiError ? fmtDiag(err.diag) : String(err), true);
      }
    });

    $("viewport").addEventListener("click", (event) => {
      if (!state.frame) return;
      const rect = event.currentTarget.getBoundingClientRect();
      const px = (event.clientX - rect.left) / rect.width * state.frame.width;
      const py = (event.clientY - rect.top) / rect.height * state.frame.height;
      const col = Math.min(state.frame.grid.cols - 1, Math.max(0, Math.floor(px / state.frame.width * state.frame.grid.cols)));
      const row = Math.min(state.frame.grid.rows - 1, Math.max(0, Math.floor(py / state.frame.height * state.frame.grid.rows)));
      state.selectedTile = row * state.frame.grid.cols + col;
      renderTileMatrix();
      updateTileInspector();
      draw();
    });

    let resizeTimer = 0;
    window.addEventListener("resize", () => {
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => { fitCanvas(); draw(); }, 100);
    });
  }

  async function main() {
    bindUI();
    try {
      await bootstrap();
    } catch (err) {
      const d = err instanceof ApiError ? err.diag : { code: "E_STAGE", message: String(err) };
      $("renderLed").className = "led err";
      log(fmtDiag(d), "err");
      toast(fmtDiag(d), true);
    }
  }

  main();
})();

