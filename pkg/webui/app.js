(() => {
  "use strict";

  const planeMeta = {
    axial: { label: "轴位", normal: "Z" },
    coronal: { label: "冠状位", normal: "Y" },
    sagittal: { label: "矢状位", normal: "X" }
  };
  const axisIndex = { X: 0, Y: 1, Z: 2 };
  const state = {
    model: null,
    mode: "locate",
    pendingPoint: null,
    busyCount: 0,
    toastTimer: null,
    windowTimer: null
  };
  let queue = Promise.resolve();

  const $ = (selector) => document.querySelector(selector);
  const $$ = (selector) => Array.from(document.querySelectorAll(selector));
  function wailsApp() {
    if (!window.go) return null;
    for (const packageName of Object.keys(window.go)) {
      const candidate = window.go[packageName] && window.go[packageName].App;
      if (candidate && typeof candidate.Invoke === "function") return candidate;
    }
    return null;
  }
  const isWails = () => wailsApp() !== null;

  async function rawInvoke(action, payload = {}) {
    const payloadText = JSON.stringify(payload || {});
    const app = wailsApp();
    if (app) {
      return await app.Invoke(action, payloadText);
    }
    const response = await fetch("/api/invoke", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action, payload: payload || {} })
    });
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    return await response.json();
  }

  async function perform(action, payload) {
    setBusy(true);
    try {
      const result = await rawInvoke(action, payload);
      if (!result || result.ok !== true) {
        const error = new Error(result?.error?.message || "操作失败");
        error.appError = result?.error || { code: "E_UNKNOWN", message: error.message };
        error.data = result?.data;
        throw error;
      }
      return result.data;
    } finally {
      setBusy(false);
    }
  }

  function invoke(action, payload) {
    const next = queue.then(() => perform(action, payload));
    queue = next.catch(() => {});
    return next;
  }

  function setBusy(active) {
    state.busyCount += active ? 1 : -1;
    if (state.busyCount < 0) state.busyCount = 0;
    $("#busy").classList.toggle("hidden", state.busyCount === 0);
  }

  function toast(message, kind = "") {
    const el = $("#toast");
    el.textContent = message;
    el.className = `toast show ${kind}`.trim();
    clearTimeout(state.toastTimer);
    state.toastTimer = setTimeout(() => {
      el.classList.remove("show");
    }, kind === "error" ? 6500 : 3500);
  }

  function showError(error) {
    const appError = error?.appError || {};
    const pieces = [appError.code || "E_ERROR", appError.message || error.message];
    if (appError.axis) pieces.push(`轴=${appError.axis}`);
    if (appError.field) pieces.push(`字段=${appError.field}`);
    if (appError.endpoint) pieces.push(`端点=${appError.endpoint}`);
    if (appError.actual !== undefined) pieces.push(`实际=${JSON.stringify(appError.actual)}`);
    if (appError.allowed !== undefined) pieces.push(`允许=${JSON.stringify(appError.allowed)}`);
    toast(pieces.filter(Boolean).join(" · "), "error");
  }

  function formatNumber(value, digits = 3) {
    if (value === null || value === undefined || Number.isNaN(Number(value))) return "--";
    return Number(value).toFixed(digits).replace(/\.?0+$/, "");
  }

  function vecText(vec, digits = 3) {
    if (!Array.isArray(vec)) return "--";
    return `(${vec.map((v) => formatNumber(v, digits)).join(", ")})`;
  }

  function formatBytesHash(hash) {
    if (!hash) return "--";
    return hash.length > 20 ? `${hash.slice(0, 10)}…${hash.slice(-8)}` : hash;
  }

  async function apiOpen(volumeIdOverride) {
    const manifestPath = $("#manifestPath").value.trim();
    if (!manifestPath) {
      toast("请填写清单路径", "error");
      return;
    }
    try {
      const data = await invoke("open", {
        manifestPath,
        volumeId: (typeof volumeIdOverride === "string" && volumeIdOverride) || $("#volumeSelect").value || undefined,
        sessionDir: $("#sessionDir").value.trim() || undefined
      });
      state.pendingPoint = null;
      applyView(data);
      if (data.recovered) {
        toast(`已从快照恢复 · sequence ${data.state.sequence}`, "ok");
      } else if (data.invalidReason) {
        toast(`旧快照已整体作废并重建：${data.invalidReason}`, "error");
      }
    } catch (error) {
      showError(error);
    }
  }

  async function apiState() {
    try {
      const data = await invoke("state");
      applyView(data);
    } catch (error) {
      showError(error);
    }
  }

  async function generateSamples() {
    const outputDir = $("#sampleDir").value.trim() || "samples";
    const seed = Number($("#sampleSeed").value) || 42;
    const manifestName = $("#sampleManifestName").value.trim() || "manifest.json";
    try {
      const result = await invoke("generate", { outputDir, seed, manifestName });
      $("#manifestPath").value = result.manifestPath;
      toast(`已生成 ${result.volumes.length} 个体数据`, "ok");
      await apiOpen(result.volumes[0] ? result.volumes[0].id : undefined);
    } catch (error) {
      showError(error);
    }
  }

  function applyView(view) {
    state.model = view;
    $("#connectionChip").textContent = isWails() ? "Wails 桌面" : "本地 HTTP";
    $("#connectionChip").className = "chip chip-ok";
    $("#volumeChip").textContent = `${view.volume.id} · ${view.volume.dimensions.join("×")}`;
    $("#volumeChip").className = "chip chip-ok";
    $("#snapshotChip").textContent = view.recovered ? "快照已恢复" : "快照已提交";
    $("#snapshotChip").className = `chip ${view.recovered ? "chip-warn" : "chip-ok"}`;

    if (document.activeElement !== $("#manifestPath")) $("#manifestPath").value = view.manifestPath;
    populateVolumeSelect(view);
    updateFormFromState(view);
    updateThicknessControls(view);
    updateSessionMeta(view);
    updateStructures(view);
    renderAllPlanes(view);
    renderAnnotations(view);
    renderMode();
  }

  function populateVolumeSelect(view) {
    const select = $("#volumeSelect");
    const current = view.volume.id;
    select.innerHTML = "";
    for (const volume of view.availableVolumes || []) {
      const option = document.createElement("option");
      option.value = volume.id;
      option.textContent = `${volume.id} · ${volume.axisOrder} · ${volume.dimensions.join("×")}`;
      if (volume.id === current) option.selected = true;
      select.appendChild(option);
    }
  }

  function updateFormFromState(view) {
    const stateData = view.state;
    if (document.activeElement !== $("#windowWidth")) $("#windowWidth").value = stateData.window.width;
    if (document.activeElement !== $("#windowLevel")) $("#windowLevel").value = stateData.window.level;
    if (document.activeElement !== $("#interpolationOrder")) {
      $("#interpolationOrder").value = String(stateData.interpolationOrder);
    }
    if (document.activeElement !== $("#crosshairX")) $("#crosshairX").value = formatNumber(stateData.crosshair[0], 3);
    if (document.activeElement !== $("#crosshairY")) $("#crosshairY").value = formatNumber(stateData.crosshair[1], 3);
    if (document.activeElement !== $("#crosshairZ")) $("#crosshairZ").value = formatNumber(stateData.crosshair[2], 3);
    $("#grayRangeLabel").textContent = `[${formatNumber(view.volume.grayRange[0], 1)}, ${formatNumber(view.volume.grayRange[1], 1)}]`;
  }

  function updateThicknessControls(view) {
    const host = $("#thicknessControls");
    const existing = host.children.length === 3;
    for (const plane of ["axial", "coronal", "sagittal"]) {
      const range = view.thicknessRanges[plane];
      const value = view.state.thicknessMM[plane];
      let row = host.querySelector(`[data-thickness-plane="${plane}"]`);
      if (!existing || !row) {
        row = document.createElement("label");
        row.className = "thickness-row";
        row.dataset.thicknessPlane = plane;
        row.innerHTML = `<span>${planeMeta[plane].label}</span><input type="number" min="0" step="0.1"><span></span>`;
        const input = row.querySelector("input");
        input.addEventListener("change", () => setThickness(plane, Number(input.value)));
        host.appendChild(row);
      }
      const input = row.querySelector("input");
      input.max = String(range.max);
      input.title = `允许 [0, ${range.max}] mm；${range.layers} 层，间距 ${range.spacing} mm`;
      if (document.activeElement !== input) input.value = value;
      row.querySelector("span:last-child").textContent = `≤${formatNumber(range.max, 1)}`;
    }
  }

  function updateSessionMeta(view) {
    $("#snapshotSequence").textContent = String(view.state.sequence);
    $("#snapshotVersion").textContent = `${view.snapshot.version}/${view.snapshot.algorithmVersion}`;
    $("#snapshotChecksum").textContent = formatBytesHash(view.snapshot.checksum);
    $("#snapshotChecksum").title = view.snapshot.checksum || "";
    $("#parallelLimits").textContent = `${view.parallelStats.maxWorkers} 线程 / ${view.parallelStats.maxPixels} px`;
    $("#parallelLimits").title = `已提交 ${view.parallelStats.submitted} 个分块，完成 ${view.parallelStats.completed} 个；峰值并发 ${view.parallelStats.peakWorkers}`;
  }

  function updateStructures(view) {
    const structureSelect = $("#structureSelect");
    const previous = structureSelect.value;
    structureSelect.innerHTML = "";
    for (const structure of view.volume.structures || []) {
      const option = document.createElement("option");
      option.value = structure.id;
      option.textContent = `${structure.label} · ${structure.id}`;
      structureSelect.appendChild(option);
    }
    if (previous && Array.from(structureSelect.options).some((o) => o.value === previous)) {
      structureSelect.value = previous;
    }
    const annotationSelect = $("#annotationSelect");
    const previousAnnotation = annotationSelect.value;
    annotationSelect.innerHTML = "";
    for (const annotation of view.state.annotations || []) {
      const option = document.createElement("option");
      option.value = annotation.id;
      option.textContent = `${annotation.id} · ${annotation.plane} · ${formatNumber(annotation.worldLengthMM)} mm`;
      annotationSelect.appendChild(option);
    }
    if (previousAnnotation && Array.from(annotationSelect.options).some((o) => o.value === previousAnnotation)) {
      annotationSelect.value = previousAnnotation;
    }
    if (annotationSelect.options.length && !annotationSelect.value) {
      annotationSelect.selectedIndex = annotationSelect.options.length - 1;
    }
  }

  function decodePixels(image) {
    if (Array.isArray(image.pixels)) return Uint8Array.from(image.pixels);
    const binary = atob(image.pixels || "");
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
    return bytes;
  }

  function renderAllPlanes(view) {
    for (const image of view.planes.images || []) {
      renderPlane(image, view);
    }
  }

  function renderPlane(image, view) {
    const canvas = document.querySelector(`[data-canvas="${image.plane}"]`);
    if (!canvas) return;
    canvas.width = image.width;
    canvas.height = image.height;
    const ctx = canvas.getContext("2d");
    const pixels = decodePixels(image);
    const imageData = ctx.createImageData(image.width, image.height);
    for (let i = 0; i < pixels.length; i += 1) {
      const value = pixels[i];
      const at = i * 4;
      imageData.data[at] = value;
      imageData.data[at + 1] = value;
      imageData.data[at + 2] = value;
      imageData.data[at + 3] = 255;
    }
    ctx.putImageData(imageData, 0, 0);
    drawOverlays(ctx, image, view);
    canvas.parentElement.querySelector(".canvas-empty").classList.add("hidden");
    layoutCanvas(canvas);
    const position = view.state.crosshair[axisIndex[planeMeta[image.plane].normal]];
    document.querySelector(`[data-position="${image.plane}"]`).textContent =
      `${planeMeta[image.plane].normal}=${formatNumber(position, 2)} mm`;
    const layerText = image.layerIndices
      .map((index, i) => `${index}:${formatNumber(image.layerWeights[i], 3)}`)
      .join(" ");
    document.querySelector(`[data-layers="${image.plane}"]`).textContent = `${image.normalAxis} ${layerText}`;
    document.querySelector(`[data-window="${image.plane}"]`).textContent =
      `WW ${formatNumber(image.window.width, 1)} / WL ${formatNumber(image.window.level, 1)}`;
  }

  function drawOverlays(ctx, image, view) {
    const [col, row] = image.crosshairPixel;
    ctx.save();
    ctx.strokeStyle = "rgba(67, 215, 192, 0.78)";
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(col + 0.5, 0);
    ctx.lineTo(col + 0.5, image.height);
    ctx.moveTo(0, row + 0.5);
    ctx.lineTo(image.width, row + 0.5);
    ctx.stroke();
    ctx.strokeStyle = "rgba(67, 215, 192, 0.95)";
    ctx.beginPath();
    ctx.arc(col + 0.5, row + 0.5, 5, 0, Math.PI * 2);
    ctx.stroke();

    for (const annotation of view.state.annotations || []) {
      if (annotation.plane !== image.plane) continue;
      const a = worldToPixel(annotation.endpointA.world, image);
      const b = worldToPixel(annotation.endpointB.world, image);
      ctx.strokeStyle = "rgba(111, 188, 255, 0.95)";
      ctx.fillStyle = "#9bd3ff";
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(a[0] + 0.5, a[1] + 0.5);
      ctx.lineTo(b[0] + 0.5, b[1] + 0.5);
      ctx.stroke();
      for (const point of [a, b]) {
        ctx.beginPath();
        ctx.arc(point[0] + 0.5, point[1] + 0.5, 2.7, 0, Math.PI * 2);
        ctx.fill();
      }
      ctx.fillStyle = "rgba(155, 211, 255, 0.95)";
      ctx.font = "8px monospace";
      ctx.fillText(`${annotation.id} · ${formatNumber(annotation.worldLengthMM, 1)}mm`, Math.min(a[0], b[0]) + 3, Math.max(9, Math.min(a[1], b[1]) - 3));
    }

    if (state.pendingPoint && state.mode === "measure") {
      const pendingImagePlane = pickPendingPlane(image, view.state.crosshair);
      if (pendingImagePlane === image.plane) {
        const a = worldToPixel(state.pendingPoint, image);
        const current = worldToPixel(view.state.crosshair, image);
        ctx.strokeStyle = "rgba(255, 190, 102, 0.95)";
        ctx.setLineDash([4, 3]);
        ctx.beginPath();
        ctx.moveTo(a[0] + 0.5, a[1] + 0.5);
        ctx.lineTo(current[0] + 0.5, current[1] + 0.5);
        ctx.stroke();
        ctx.setLineDash([]);
        ctx.fillStyle = "#ffcf86";
        ctx.beginPath();
        ctx.arc(a[0] + 0.5, a[1] + 0.5, 3, 0, Math.PI * 2);
        ctx.fill();
      }
    }
    ctx.restore();
  }

  function pickPendingPlane(image, crosshair) {
    return state.pendingPlane || image.plane;
  }

  function worldToPixel(world, image) {
    const uAxis = axisIndex[image.uAxis];
    const vAxis = axisIndex[image.vAxis];
    const col = clamp(Math.round(world[uAxis] / image.uSpacingMM), 0, image.width - 1);
    const row = clamp(Math.round(world[vAxis] / image.vSpacingMM), 0, image.height - 1);
    return [col, row];
  }

  function layoutCanvas(canvas) {
    const wrap = canvas.parentElement;
    const availableWidth = Math.max(1, wrap.clientWidth - 12);
    const availableHeight = Math.max(1, wrap.clientHeight - 12);
    const scale = Math.min(availableWidth / canvas.width, availableHeight / canvas.height);
    const snapped = Math.max(0.25, Math.floor(scale * 4) / 4);
    canvas.style.width = `${Math.max(1, canvas.width * snapped)}px`;
    canvas.style.height = `${Math.max(1, canvas.height * snapped)}px`;
  }

  function renderAnnotations(view) {
    const tbody = $("#annotationRows");
    tbody.innerHTML = "";
    const annotations = view.state.annotations || [];
    if (!annotations.length) {
      tbody.innerHTML = '<tr><td colspan="5" class="empty-row">尚无标注</td></tr>';
    }
    for (const annotation of annotations) {
      const tr = document.createElement("tr");
      tr.innerHTML = `
        <td><strong>${annotation.id}</strong><br><span class="dual"><em>${annotation.plane}</em></span></td>
        <td>${formatNumber(annotation.worldLengthMM, 4)} mm</td>
        <td>${formatNumber(annotation.voxelLength, 4)} vx</td>
        <td><span class="dual">W ${vecText(annotation.endpointA.world)}</span><span class="dual"><em>V ${vecText(annotation.endpointA.voxel)}</em></span></td>
        <td><span class="dual">W ${vecText(annotation.endpointB.world)}</span><span class="dual"><em>V ${vecText(annotation.endpointB.voxel)}</em></span></td>`;
      tbody.appendChild(tr);
    }
    const pending = $("#pendingMeasure");
    if (state.pendingPoint) {
      pending.classList.remove("hidden");
      pending.textContent = `端点 A 世界 ${vecText(state.pendingPoint)} · 请在 ${state.pendingPlane || "当前"} 平面选择端点 B`;
    } else {
      pending.classList.add("hidden");
    }
    const select = $("#annotationSelect");
    if (annotations.length && !select.value) select.selectedIndex = annotations.length - 1;
  }

  function renderMode() {
    $("#locateModeBtn").classList.toggle("active", state.mode === "locate");
    $("#measureModeBtn").classList.toggle("active", state.mode === "measure");
    $("#modeHint").textContent = state.mode === "locate"
      ? "单击任一平面更新共享世界坐标定位点。"
      : "第一次单击确定端点 A，第二次单击同平面确定端点 B；坐标按世界/体素双读数提交。";
  }

  async function setWindow(width, level) {
    try {
      const data = await invoke("set_window", { width, level });
      applyView(data);
    } catch (error) {
      showError(error);
    }
  }

  async function setThickness(plane, thicknessMM) {
    try {
      const data = await invoke("set_thickness", { plane, thicknessMM });
      applyView(data);
    } catch (error) {
      showError(error);
      if (state.model) updateThicknessControls(state.model);
    }
  }

  async function setCrosshair(point) {
    try {
      const data = await invoke("set_crosshair", { x: point[0], y: point[1], z: point[2] });
      applyView(data);
      return data;
    } catch (error) {
      showError(error);
      return null;
    }
  }

  function crosshairFromInputs() {
    return [
      Number($("#crosshairX").value),
      Number($("#crosshairY").value),
      Number($("#crosshairZ").value)
    ];
  }

  function canvasClick(event) {
    if (!state.model) return;
    const canvas = event.currentTarget;
    const image = state.model.planes.images.find((item) => item.plane === canvas.dataset.canvas);
    if (!image) return;
    const rect = canvas.getBoundingClientRect();
    const col = clamp(Math.floor((event.clientX - rect.left) / rect.width * image.width), 0, image.width - 1);
    const row = clamp(Math.floor((event.clientY - rect.top) / rect.height * image.height), 0, image.height - 1);
    const point = [...state.model.state.crosshair];
    point[axisIndex[image.uAxis]] = col * image.uSpacingMM;
    point[axisIndex[image.vAxis]] = row * image.vSpacingMM;

    if (state.mode === "measure") {
      if (!state.pendingPoint) {
        state.pendingPoint = point;
        state.pendingPlane = image.plane;
        setCrosshair(point);
        if (state.model) renderAnnotations(state.model);
        return;
      }
      if (state.pendingPlane !== image.plane) {
        toast("测距端点必须位于同一平面；请取消或切换回 A 所在平面。", "error");
        return;
      }
      addMeasurement(image.plane, state.pendingPoint, point);
      return;
    }
    setCrosshair(point);
  }

  async function addMeasurement(plane, pointA, pointB) {
    try {
      await invoke("set_crosshair", { x: pointB[0], y: pointB[1], z: pointB[2] });
      const data = await invoke("add_annotation", {
        plane,
        a: { x: pointA[0], y: pointA[1], z: pointA[2] },
        b: { x: pointB[0], y: pointB[1], z: pointB[2] },
        coordinateSystem: "world"
      });
      state.pendingPoint = null;
      state.pendingPlane = null;
      applyView(data);
      const latest = data.state.annotations[data.state.annotations.length - 1];
      if (latest) {
        const select = $("#annotationSelect");
        select.value = latest.id;
        toast(`已提交 ${latest.id} · ${formatNumber(latest.worldLengthMM, 4)} mm`, "ok");
      }
    } catch (error) {
      showError(error);
    }
  }

  async function deleteLast() {
    if (!state.model || !state.model.state.annotations.length) return;
    const id = state.model.state.annotations[state.model.state.annotations.length - 1].id;
    try {
      const data = await invoke("delete_annotation", { annotationId: id });
      applyView(data);
    } catch (error) {
      showError(error);
    }
  }

  async function autoBoundary() {
    const structureId = $("#structureSelect").value;
    if (!structureId) return;
    try {
      const data = await invoke("auto_annotation", { structureId });
      applyView(data);
      const latest = data.state.annotations[data.state.annotations.length - 1];
      if (latest) {
        $("#annotationSelect").value = latest.id;
        toast(`已载入 ${latest.id}：端点来自解析结构边界`, "ok");
      }
    } catch (error) {
      showError(error);
    }
  }

  async function precisionCheck() {
    const annotationId = $("#annotationSelect").value;
    if (!annotationId) {
      toast("请先创建或选择标注", "error");
      return;
    }
    try {
      const report = await invoke("precision", {
        annotationId,
        structureId: $("#structureSelect").value || undefined,
        toleranceMM: Number($("#toleranceInput").value) || 2.5
      });
      renderPrecision(report);
      toast(report.pass ? "精度校验通过" : "精度校验未通过", report.pass ? "ok" : "error");
    } catch (error) {
      if (error.data) renderPrecision(error.data);
      showError(error);
    }
  }

  function renderPrecision(report) {
    const host = $("#precisionReport");
    if (!report) {
      host.className = "precision-report empty";
      host.textContent = "等待精度校验";
      return;
    }
    host.className = `precision-report ${report.pass ? "pass" : "fail"}`;
    host.innerHTML = `
      <div class="report-head"><strong>${report.pass ? "PASS" : "E_PREC"} · ${report.mode}</strong><span>${report.structureId}</span></div>
      <div class="report-grid">
        <span>实测长度</span><span>${formatNumber(report.measuredLengthMM, 6)} mm</span>
        <span>解析真值</span><span>${formatNumber(report.truthLengthMM, 6)} mm</span>
        <span>绝对偏差</span><span>${formatNumber(report.deviationMM, 6)} mm</span>
        <span>推导上界</span><span>${formatNumber(report.upperBoundMM, 6)} mm</span>
        <span>声明容差</span><span>${formatNumber(report.toleranceMM, 6)} mm</span>
        <span>边界残差</span><span>${formatNumber(report.boundaryResidualMM, 6)} mm</span>
      </div>
      <div class="report-formula">${report.formula}<br>${(report.inputs || []).join("<br>")}</div>`;
  }

  async function cancelOperation() {
    try {
      await rawInvoke("cancel", {});
      toast("已发送取消信号；未提交草稿会丢弃", "error");
    } catch (error) {
      showError(error);
    }
  }

  async function pauseSession() {
    try {
      const result = await invoke("pause");
      toast(`已暂停于已提交 sequence ${result.sequence}；可直接继续操作或退出`, "ok");
    } catch (error) {
      showError(error);
    }
  }

  function bindEvents() {
    $("#openBtn").addEventListener("click", () => apiOpen());
    $("#reopenBtn").addEventListener("click", () => apiOpen());
    $("#volumeSelect").addEventListener("change", () => apiOpen());
    $("#stateBtn").addEventListener("click", apiState);
    $("#generateBtn").addEventListener("click", generateSamples);
    $("#pauseBtn").addEventListener("click", pauseSession);
    $("#cancelBtn").addEventListener("click", cancelOperation);
    $("#deleteLastBtn").addEventListener("click", deleteLast);
    $("#autoBoundaryBtn").addEventListener("click", autoBoundary);
    $("#precisionBtn").addEventListener("click", precisionCheck);
    $("#locateModeBtn").addEventListener("click", () => {
      state.mode = "locate";
      state.pendingPoint = null;
      state.pendingPlane = null;
      if (state.model) renderAllPlanes(state.model);
      renderMode();
    });
    $("#measureModeBtn").addEventListener("click", () => {
      state.mode = "measure";
      state.pendingPoint = null;
      state.pendingPlane = null;
      if (state.model) renderAllPlanes(state.model);
      renderMode();
    });
    for (const canvas of $$("canvas[data-canvas]")) {
      canvas.addEventListener("click", canvasClick);
    }
    $("#interpolationOrder").addEventListener("change", async () => {
      try {
        const data = await invoke("set_interpolation", { order: Number($("#interpolationOrder").value) });
        applyView(data);
      } catch (error) {
        showError(error);
      }
    });
    const scheduleWindow = () => {
      clearTimeout(state.windowTimer);
      state.windowTimer = setTimeout(() => {
        const width = Number($("#windowWidth").value);
        const level = Number($("#windowLevel").value);
        if (Number.isFinite(width) && Number.isFinite(level)) setWindow(width, level);
      }, 180);
    };
    $("#windowWidth").addEventListener("input", scheduleWindow);
    $("#windowLevel").addEventListener("input", scheduleWindow);
    $("#crosshairX").addEventListener("change", () => setCrosshair(crosshairFromInputs()));
    $("#crosshairY").addEventListener("change", () => setCrosshair(crosshairFromInputs()));
    $("#crosshairZ").addEventListener("change", () => setCrosshair(crosshairFromInputs()));
    window.addEventListener("resize", () => {
      for (const canvas of $$("canvas[data-canvas]")) layoutCanvas(canvas);
    });
  }

  async function boot() {
    bindEvents();
    renderMode();
    $("#connectionChip").textContent = isWails() ? "Wails 初始化" : "HTTP 初始化";
    try {
      const status = await invoke("status");
      if (status.manifestPath) $("#manifestPath").value = status.manifestPath;
      if (status.sessionDir) $("#sessionDir").value = status.sessionDir;
      if (status.open) {
        await apiState();
      } else {
        await apiOpen();
      }
    } catch (error) {
      $("#connectionChip").textContent = "连接失败";
      $("#connectionChip").className = "chip chip-warn";
      showError(error);
    }
  }

  function clamp(value, min, max) {
    return Math.max(min, Math.min(max, value));
  }

  boot();
})();
