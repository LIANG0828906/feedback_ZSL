(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const api = () => (window.go && window.go.main && window.go.main.App) || null;

  let current = null;
  let selectedLight = "";
  let selectedBlock = null;
  let controlsInitialized = false;
  let lastDiagnostic = "ready";
  let busyCount = 0;

  const STATUS_SWATCH = {
    pending: "#5c6b80",
    invalidated: "#f0b35a",
    computed: "#5cd8e8",
    committed: "#5cd8e8",
    failed: "#f06d72",
    reused: "#62d68b",
  };

  function setBusy(on) {
    busyCount += on ? 1 : -1;
    if (busyCount < 0) busyCount = 0;
    $("busy").classList.toggle("hidden", busyCount === 0);
  }

  function setMessage(text) {
    $("message").textContent = text || "";
  }

  function fmt(x, digits = 4) {
    if (x === undefined || x === null || !Number.isFinite(Number(x))) return "—";
    const n = Number(x);
    if (n !== 0 && (Math.abs(n) >= 10000 || Math.abs(n) < 0.0001)) return n.toExponential(3);
    return n.toFixed(digits).replace(/\.?0+$/, "");
  }

  function shortHash(value) {
    if (!value) return "—";
    return String(value).slice(0, 12) + "…";
  }

  async function call(method, ...args) {
    const a = api();
    if (!a) {
      lastDiagnostic = "Wails bindings are unavailable. Launch with `wails dev -tags desktop` or `wails build -tags desktop`.";
      renderDiagnostics();
      throw new Error(lastDiagnostic);
    }
    setBusy(true);
    try {
      const result = await a[method](...args);
      applyResult(result, method);
      return result;
    } catch (err) {
      lastDiagnostic = "IPC error:\n" + (err && err.message ? err.message : String(err));
      renderDiagnostics();
      setMessage("IPC error");
      throw err;
    } finally {
      setBusy(false);
    }
  }

  function applyResult(result, method) {
    if (!result) {
      lastDiagnostic = "empty result from " + method;
      renderDiagnostics();
      return;
    }
    if (result.view) {
      current = result.view;
    }
    if (result.ok) {
      const msg = result.view && result.view.message ? result.view.message : method + " completed";
      lastDiagnostic = msg;
      setMessage(msg);
    } else {
      const e = result.error || {};
      const where = [];
      if (e.light_id) where.push("light=" + e.light_id);
      if (e.field) where.push("field=" + e.field);
      if (e.block_x !== undefined) where.push(`block=(${e.block_x},${e.block_y})`);
      if (e.pixel_x !== undefined) where.push(`pixel=(${e.pixel_x},${e.pixel_y})`);
      if (e.item) where.push("item=" + e.item);
      lastDiagnostic =
        `${e.code || "E_IO"}: ${e.message || "operation failed"}` +
        (where.length ? "\n" + where.join(" · ") : "") +
        (e.left !== undefined ? `\nleft=${e.left}\nright=${e.right}` : "");
      setMessage((e.code || "E_IO") + " · " + (e.message || method));
    }
    if (result.view) {
      populateControls(result, method);
      renderAll();
      if (selectedBlock) loadBlockDetail(selectedBlock.x, selectedBlock.y, true);
    } else {
      renderDiagnostics();
    }
  }

  function renderAll() {
    if (!current) return;
    renderMetrics();
    drawScene();
    drawDepthGrids();
    drawHeatmap($("volumeCanvas"), current.image.volume, "volume");
    drawHeatmap($("diffCanvas"), current.image.truth_diff, "diff");
    drawHeatmap($("compositeCanvas"), current.image.composite, "volume");
    renderInvalidations();
    renderBlockChips();
    renderDiagnostics();
    $("restoreSummary").textContent = current.state.restore_summary || "—";
  }

  function renderMetrics() {
    const s = current.state;
    $("sourceHash").textContent = shortHash(s.source_hash);
    $("sourceHash").title = s.source_hash || "";
    $("sceneHash").textContent = shortHash(s.scene_hash);
    $("sceneHash").title = s.scene_hash || "";
    $("cacheStat").textContent = `${s.cache_count} / ${s.total_blocks}`;
    $("pendingStat").textContent = String(s.pending_count);
    const ck = s.checkpoint || {};
    $("ckptStat").textContent = `${ck.blocks || 0} b · ${ck.batches || 0} batches · ${ck.history || 0} hist`;
    const q = (current.last_report && current.last_report.quality) || s.quality || {};
    $("boundStat").textContent = q.error_bound_per_pixel !== undefined ? fmt(q.error_bound_per_pixel, 6) : "—";
    $("boundStat").title = q.formula || "";
    const edit = current.last_edit;
    $("editStat").textContent = edit ? `${edit.affected_blocks} affected / ${edit.reused_blocks} reused` : "—";
  }

  function populateControls(result, method) {
    const s = current.state;
    const lightSelect = $("lightSelect");
    const oldLights = Array.from(lightSelect.options).map((o) => o.value);
    const lightIDs = s.lights.map((l) => l.id);
    if (oldLights.join("|") !== lightIDs.join("|")) {
      lightSelect.innerHTML = "";
      for (const id of lightIDs) {
        const opt = document.createElement("option");
        opt.value = id;
        opt.textContent = id;
        lightSelect.appendChild(opt);
      }
    }
    if (!selectedLight && lightIDs.length) selectedLight = lightIDs[0];
    lightSelect.value = selectedLight;
    fillLightEditor();
    if (!controlsInitialized || ["Bootstrap", "Configure", "RenderPreview"].includes(method)) {
      fillMediumEditor();
      fillConfig();
      controlsInitialized = true;
    }
    if (method === "Bootstrap") {
      $("scenePath").value = current.scene_path || "";
      $("dbPath").value = current.db_path || "";
    }
  }

  function selectedLightData() {
    if (!current) return null;
    return current.state.lights.find((l) => l.id === selectedLight) || null;
  }

  function fillLightEditor() {
    const l = selectedLightData();
    if (!l) return;
    $("intR").value = l.intensity[0];
    $("intG").value = l.intensity[1];
    $("intB").value = l.intensity[2];
    $("dirX").value = l.direction[0];
    $("dirY").value = l.direction[1];
    $("dirZ").value = l.direction[2];
    $("anisotropy").value = l.anisotropy;
    const cov = current.coverage && current.coverage[selectedLight] ? current.coverage[selectedLight].length : 0;
    $("lightCoverageHint").textContent = `${l.type} · ${cov} influence blocks`;
  }

  function fillMediumEditor() {
    const m = current.state.medium;
    $("extR").value = m.extinction[0];
    $("extG").value = m.extinction[1];
    $("extB").value = m.extinction[2];
    $("sctR").value = m.scattering[0];
    $("sctG").value = m.scattering[1];
    $("sctB").value = m.scattering[2];
    $("mediumG").value = m.phase_g;
  }

  function fillConfig() {
    const p = current.state.params;
    $("cfgStep").value = p.step;
    $("cfgJitterSamples").value = p.jitter_samples;
    $("cfgWorkers").value = p.max_workers;
    $("cfgSlots").value = p.buffer_slots;
    $("cfgBatch").value = p.batch_size;
    $("cfgSampleCap").value = p.max_samples_per_thread;
    $("cfgJitter").value = p.jitter;
  }

  function number(id) {
    return Number($(id).value);
  }

  async function applyLight() {
    const l = selectedLightData();
    if (!l) return;
    await call("ApplyEdit", {
      light_id: selectedLight,
      intensity: [number("intR"), number("intG"), number("intB")],
      direction: [number("dirX"), number("dirY"), number("dirZ")],
      anisotropy: number("anisotropy"),
    });
  }

  async function applyMedium() {
    await call("ApplyEdit", {
      medium: {
        extinction: [number("extR"), number("extG"), number("extB")],
        scattering: [number("sctR"), number("sctG"), number("sctB")],
        phase_g: number("mediumG"),
      },
    });
  }

  async function applyConfig() {
    await call("Configure", {
      step: number("cfgStep"),
      jitter: $("cfgJitter").value,
      jitter_samples: number("cfgJitterSamples"),
      max_workers: number("cfgWorkers"),
      max_samples_per_thread: number("cfgSampleCap"),
      buffer_slots: number("cfgSlots"),
      batch_size: number("cfgBatch"),
    });
  }

  function setupCanvas(canvas) {
    const rect = canvas.getBoundingClientRect();
    const dpr = window.devicePixelRatio || 1;
    const w = Math.max(1, Math.floor(rect.width));
    const h = Math.max(1, Math.floor(rect.height));
    if (canvas.width !== Math.floor(w * dpr) || canvas.height !== Math.floor(h * dpr)) {
      canvas.width = Math.floor(w * dpr);
      canvas.height = Math.floor(h * dpr);
    }
    const ctx = canvas.getContext("2d");
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    return { ctx, w, h };
  }

  function drawScene() {
    const { ctx, w, h } = setupCanvas($("sceneCanvas"));
    const s = current.state;
    ctx.fillStyle = "#060a10";
    ctx.fillRect(0, 0, w, h);
    const pad = 18;
    const xmin = -8.5, xmax = 8.5, zmin = -18, zmax = 8;
    const mx = (x) => pad + ((x - xmin) / (xmax - xmin)) * (w - pad * 2);
    const mz = (z) => pad + ((z - zmin) / (zmax - zmin)) * (h - pad * 2);

    ctx.strokeStyle = "#132031";
    ctx.lineWidth = 1;
    for (let x = -8; x <= 8; x += 2) {
      ctx.beginPath();
      ctx.moveTo(mx(x), pad);
      ctx.lineTo(mx(x), h - pad);
      ctx.stroke();
    }
    for (let z = -18; z <= 8; z += 2) {
      ctx.beginPath();
      ctx.moveTo(pad, mz(z));
      ctx.lineTo(w - pad, mz(z));
      ctx.stroke();
    }
    ctx.fillStyle = "#40536b";
    ctx.font = "9px monospace";
    ctx.fillText("X", w - 20, mz(0) - 5);
    ctx.fillText("Z", mx(0) + 5, h - 8);

    for (const m of s.geometry) {
      const x0 = m.origin[0], x1 = m.origin[0] + m.axis_u[0];
      const z = m.origin[2];
      const y0 = m.origin[1], y1 = m.origin[1] + m.axis_v[1];
      const cx = (x0 + x1) / 2;
      ctx.strokeStyle = colorCSS(m.color, 0.95);
      ctx.lineWidth = 5;
      ctx.beginPath();
      ctx.moveTo(mx(x0), mz(z));
      ctx.lineTo(mx(x1), mz(z));
      ctx.stroke();
      ctx.fillStyle = "#8ca5bf";
      ctx.fillText(`${m.id}  y[${fmt(y0, 1)},${fmt(y1, 1)}] z=${fmt(z, 1)}`, mx(cx) - 45, mz(z) - 7);
    }

    for (const c of s.camera_path) {
      const x = mx(c.position[0]), z = mz(c.position[2]);
      ctx.fillStyle = c.id === s.active_camera_id ? "#d7f8ff" : "#4b627b";
      ctx.beginPath();
      ctx.moveTo(x, z - 6);
      ctx.lineTo(x - 5, z + 4);
      ctx.lineTo(x + 5, z + 4);
      ctx.closePath();
      ctx.fill();
      ctx.fillStyle = "#67809b";
      ctx.fillText(c.id, x + 7, z + 2);
      ctx.strokeStyle = "#2b4058";
      ctx.beginPath();
      ctx.moveTo(x, z);
      ctx.lineTo(mx(c.target[0]), mz(c.target[2]));
      ctx.stroke();
    }

    for (const l of s.lights) {
      const x = mx(l.position[0]), z = mz(l.position[2]);
      const r = Math.max(4, Math.abs(mx(l.radius) - mx(0)));
      ctx.fillStyle = colorCSS(l.intensity, 0.12);
      ctx.strokeStyle = selectedLight === l.id ? "#ffffff" : colorCSS(l.intensity, 0.8);
      ctx.lineWidth = selectedLight === l.id ? 2 : 1;
      ctx.beginPath();
      ctx.arc(x, z, r, 0, Math.PI * 2);
      ctx.fill();
      ctx.stroke();
      ctx.fillStyle = colorCSS(l.intensity, 1);
      ctx.beginPath();
      ctx.arc(x, z, 4, 0, Math.PI * 2);
      ctx.fill();
      ctx.fillStyle = selectedLight === l.id ? "#ffffff" : "#c5d7e9";
      ctx.fillText(l.id, x + 7, z - 7);
      const dx = l.direction[0], dz = l.direction[2];
      const len = Math.hypot(dx, dz) || 1;
      ctx.strokeStyle = "rgba(255,255,255,.35)";
      ctx.beginPath();
      ctx.moveTo(x, z);
      ctx.lineTo(x + (dx / len) * 18, z + (dz / len) * 18);
      ctx.stroke();
    }
  }

  function drawDepthGrids() {
    const { ctx, w, h } = setupCanvas($("depthCanvas"));
    const meshes = current.state.geometry;
    if (!meshes.length) return;
    const panelW = w / meshes.length;
    meshes.forEach((m, mi) => {
      const ox = mi * panelW + 8;
      const oy = 22;
      const availW = panelW - 16;
      const availH = h - 34;
      const cellW = availW / m.columns;
      const cellH = availH / m.rows;
      let min = Infinity, max = -Infinity;
      for (const row of m.depths) for (const d of row) { min = Math.min(min, d); max = Math.max(max, d); }
      if (Math.abs(max - min) < 1e-9) { min -= 0.01; max += 0.01; }
      ctx.fillStyle = "#c3d5e7";
      ctx.font = "10px monospace";
      ctx.fillText(`${m.id}  ${m.columns}×${m.rows}`, ox, 13);
      for (let r = 0; r < m.rows; r++) {
        for (let c = 0; c < m.columns; c++) {
          const t = (m.depths[r][c] - min) / (max - min);
          ctx.fillStyle = depthColor(t);
          ctx.fillRect(ox + c * cellW, oy + r * cellH, Math.max(1, cellW - 1), Math.max(1, cellH - 1));
          ctx.fillStyle = t > 0.58 ? "#081018" : "#d8edf7";
          ctx.font = "8px monospace";
          ctx.fillText(fmt(m.depths[r][c], 2), ox + c * cellW + 2, oy + r * cellH + cellH * 0.62);
        }
      }
      ctx.fillStyle = "#6f849b";
      ctx.font = "8px monospace";
      ctx.fillText(`normal ${m.normal.map((v) => fmt(v, 1)).join("/")}`, ox, h - 5);
    });
  }

  function depthColor(t) {
    const stops = [[18, 41, 68], [34, 130, 160], [76, 206, 190], [240, 180, 92], [238, 96, 105]];
    const pos = Math.max(0, Math.min(0.999, t)) * (stops.length - 1);
    const i = Math.floor(pos), f = pos - i;
    const a = stops[i], b = stops[i + 1];
    return `rgb(${Math.round(a[0] + (b[0] - a[0]) * f)},${Math.round(a[1] + (b[1] - a[1]) * f)},${Math.round(a[2] + (b[2] - a[2]) * f)})`;
  }

  function colorCSS(c, mul) {
    const m = Math.max(c[0], c[1], c[2]) || 1;
    return `rgba(${Math.round(255 * Math.min(1, c[0] / m))},${Math.round(255 * Math.min(1, c[1] / m))},${Math.round(255 * Math.min(1, c[2] / m))},${mul})`;
  }

  function drawHeatmap(canvas, data, mode) {
    if (!current) return;
    const { ctx, w, h } = setupCanvas(canvas);
    const iw = current.image.width, ih = current.image.height;
    const temp = document.createElement("canvas");
    temp.width = iw;
    temp.height = ih;
    const tctx = temp.getContext("2d");
    const img = tctx.createImageData(iw, ih);
    let diffScale = 1;
    if (mode === "diff") {
      let maxDiff = 0;
      for (let i = 0; i < data.length; i++) maxDiff = Math.max(maxDiff, Math.abs(data[i]));
      if (maxDiff > 1e-12) diffScale = 1 / maxDiff;
    }
    for (let i = 0; i < iw * ih; i++) {
      const off = i * 3;
      let r, g, b;
      if (mode === "volume") {
        r = tone(data[off], 10);
        g = tone(data[off + 1], 10);
        b = tone(data[off + 2], 10);
      } else {
        const v = Math.min(1, (Math.max(data[off], data[off + 1], data[off + 2]) || 0) * diffScale);
        [r, g, b] = heat(v);
      }
      img.data[i * 4] = Math.round(255 * r);
      img.data[i * 4 + 1] = Math.round(255 * g);
      img.data[i * 4 + 2] = Math.round(255 * b);
      img.data[i * 4 + 3] = 255;
    }
    tctx.putImageData(img, 0, 0);
    ctx.imageSmoothingEnabled = false;
    ctx.drawImage(temp, 0, 0, w, h);

    const scaleX = w / iw, scaleY = h / ih;
    const bw = current.state.block_size.width * scaleX;
    const bh = current.state.block_size.height * scaleY;
    const statuses = new Map();
    for (const b of current.state.blocks) statuses.set(`${b.block_x},${b.block_y}`, b);
    const covered = coverageSet(selectedLight);
    const affected = affectedSet();
    const reused = new Set((current.last_report && current.last_report.reused_block_keys || []).map((k) => `${k.x},${k.y}`));
    const selected = selectedBlock ? `${selectedBlock.x},${selectedBlock.y}` : "";

    for (const [id, b] of statuses) {
      const [bx, by] = id.split(",").map(Number);
      const x = bx * bw, y = by * bh;
      let stroke = "#60738a";
      if (b.failed) stroke = STATUS_SWATCH.failed;
      else if (b.invalidated || affected.has(id)) stroke = STATUS_SWATCH.invalidated;
      else if (b.cache_hit || reused.has(id)) stroke = STATUS_SWATCH.reused;
      else if (b.state === "computed" || b.state === "committed") stroke = STATUS_SWATCH.computed;
      ctx.strokeStyle = stroke;
      ctx.globalAlpha = affected.has(id) ? 0.95 : 0.55;
      ctx.lineWidth = affected.has(id) ? 2 : 1;
      ctx.strokeRect(x + 0.5, y + 0.5, bw - 1, bh - 1);
      if (covered.has(id)) {
        ctx.globalAlpha = 0.9;
        ctx.strokeStyle = "#b38cff";
        ctx.setLineDash([4, 3]);
        ctx.strokeRect(x + 3, y + 3, Math.max(1, bw - 6), Math.max(1, bh - 6));
        ctx.setLineDash([]);
      }
      if (id === selected) {
        ctx.globalAlpha = 1;
        ctx.strokeStyle = "#ffffff";
        ctx.lineWidth = 2;
        ctx.strokeRect(x + 1, y + 1, Math.max(1, bw - 2), Math.max(1, bh - 2));
      }
    }
    ctx.globalAlpha = 1;

    if (mode === "volume") {
      const ctrls = [["computed", "#5cd8e8"], ["reused", "#62d68b"], ["invalidated", "#f0b35a"], ["failed", "#f06d72"], ["selected light", "#b38cff"]];
      $("statusLegend").innerHTML = ctrls.map(([label, color]) => `<span style="--sw:${color}">${label}</span>`).join("");
    }
  }

  function tone(v, exposure) {
    return Math.max(0, Math.min(1, 1 - Math.exp(-Math.max(0, v) * exposure)));
  }

  function heat(v) {
    if (v <= 0) return [0.02, 0.03, 0.05];
    if (v < 0.33) return [0.35 * v * 3, 0.02, 0.11];
    if (v < 0.66) {
      const t = (v - 0.33) / 0.33;
      return [0.35 + 0.6 * t, 0.02 + 0.45 * t, 0.06];
    }
    const t = (v - 0.66) / 0.34;
    return [0.95, 0.47 + 0.5 * t, 0.08 + 0.82 * t];
  }

  function coverageSet(lightID) {
    const out = new Set();
    const list = current && current.coverage && current.coverage[lightID];
    if (list) for (const k of list) out.add(`${k.x},${k.y}`);
    return out;
  }

  function affectedSet() {
    const out = new Set();
    const inv = (current.last_report ? current.last_report.affected : null) ||
      (current.last_edit && current.last_edit.invalidated) || [];
    for (const i of inv) if (i.stage === "volume") out.add(`${i.block_x},${i.block_y}`);
    return out;
  }

  function renderInvalidations() {
    const inv = (current.last_report ? current.last_report.affected : null) ||
      (current.last_edit && current.last_edit.invalidated) || [];
    const list = $("invalidations");
    if (!inv.length) {
      list.className = "invalidation-list empty";
      list.textContent = "No invalidation recorded.";
      $("propagationSummary").textContent = "—";
      return;
    }
    const volume = inv.filter((i) => i.stage === "volume");
    const blend = inv.filter((i) => i.stage === "blend");
    $("propagationSummary").textContent =
      `${volume.length} volume → ${blend.length} blend · depth ${Math.max(...inv.map((i) => i.impact_depth))}`;
    list.className = "invalidation-list";
    list.innerHTML = inv.slice(0, 140).map((i) => {
      const src = (i.source_lights || []).concat(i.source_occluders || []).join(",");
      return `<div class="inv-item">
        <span class="inv-stage">d${i.impact_depth} ${escapeHTML(i.stage)}</span>
        <span class="inv-block">${i.block_x},${i.block_y}</span>
        <span class="inv-reason" title="${escapeHTML(i.reason)}">${escapeHTML(i.reason)} <span class="inv-source">${escapeHTML(src)}</span></span>
      </div>`;
    }).join("");
  }

  function renderBlockChips() {
    const container = $("blockChips");
    const blocks = [...current.state.blocks].sort((a, b) => (a.block_y - b.block_y) || (a.block_x - b.block_x));
    const covered = coverageSet(selectedLight);
    const affected = affectedSet();
    container.innerHTML = blocks.map((b) => {
      const id = `${b.block_x},${b.block_y}`;
      let cls = "block-chip";
      if (b.failed) cls += " failed";
      else if (b.invalidated || affected.has(id)) cls += " invalidated";
      else if (b.cache_hit) cls += " reused";
      else if (b.state === "computed" || b.state === "committed") cls += " computed";
      if (covered.has(id)) cls += " covered";
      if (selectedBlock && selectedBlock.x === b.block_x && selectedBlock.y === b.block_y) cls += " selected";
      const tip = `${id} · ${b.state || "pending"} · ${fmt(b.duration_ms, 2)} ms${b.reasons ? " · " + b.reasons.join(",") : ""}`;
      return `<div class="${cls}" data-x="${b.block_x}" data-y="${b.block_y}" title="${escapeHTML(tip)}">${b.block_x},${b.block_y}</div>`;
    }).join("");
    for (const chip of container.querySelectorAll(".block-chip")) {
      chip.addEventListener("click", () => {
        const x = Number(chip.dataset.x), y = Number(chip.dataset.y);
        selectedBlock = { x, y };
        loadBlockDetail(x, y, false);
        renderAll();
      });
    }
  }

  async function loadBlockDetail(x, y, quiet) {
    if (!api()) return;
    const detail = await api().Block(x, y);
    if (!detail || !detail.found) {
      $("blockInfo").textContent = `block (${x},${y}) is not cached yet`;
      $("blockTitle").textContent = `(${x},${y})`;
      drawBlockPreview(null);
      return;
    }
    $("blockTitle").textContent = `(${x},${y}) · ${detail.view.state}`;
    const lines = [
      `state       ${detail.view.state}`,
      `cache hit   ${detail.view.cache_hit}`,
      `invalidated ${detail.view.invalidated}`,
      `reasons     ${(detail.view.reasons || []).join(", ") || "—"}`,
      `sources     ${(detail.view.source_lights || []).concat(detail.view.source_occluders || []).join(", ") || "—"}`,
      `used lights ${(detail.view.used_lights || []).join(", ") || "—"}`,
      `occluders   ${(detail.used_occluders || []).join(", ") || "—"}`,
      `digest      ${shortHash(detail.view.digest)}`,
      `samples     ${detail.view.samples}`,
      `actual step ${fmt(detail.view.actual_step, 5)}`,
      `duration    ${fmt(detail.view.duration_ms, 3)} ms`,
      `mean T      [${detail.stats.mean_transmittance.map((v) => fmt(v, 4)).join(", ")}]`,
      `mean S      [${detail.stats.mean_scatter.map((v) => fmt(v, 4)).join(", ")}]`,
      `max S       [${detail.stats.max_scatter.map((v) => fmt(v, 4)).join(", ")}]`,
    ];
    $("blockInfo").textContent = lines.join("\n");
    drawBlockPreview(detail);
    if (!quiet) setMessage(`inspected block (${x},${y})`);
  }

  function drawBlockPreview(detail) {
    const { ctx, w, h } = setupCanvas($("blockCanvas"));
    if (!detail) {
      ctx.fillStyle = "#4f6379";
      ctx.font = "11px monospace";
      ctx.fillText("no block data", 20, 30);
      return;
    }
    const half = w / 2;
    const cw = half / detail.width;
    const ch = h / detail.height;
    const off = Math.max(4, cw * 0.08);
    for (let i = 0; i < detail.pixels.length; i++) {
      const p = detail.pixels[i];
      const col = p.x % detail.width;
      const row = Math.floor(i / detail.width);
      const baseX = col * cw;
      const y = row * ch;
      const v = p.volume;
      ctx.fillStyle = `rgb(${Math.round(255 * tone(v[0], 10))},${Math.round(255 * tone(v[1], 10))},${Math.round(255 * tone(v[2], 10))})`;
      ctx.fillRect(baseX + off, y + off, cw - off * 2, ch - off * 2);
      const c = detail.composite[i] || [0, 0, 0];
      ctx.fillStyle = `rgb(${Math.round(255 * tone(c[0], 10))},${Math.round(255 * tone(c[1], 10))},${Math.round(255 * tone(c[2], 10))})`;
      ctx.fillRect(half + baseX + off, y + off, cw - off * 2, ch - off * 2);
    }
    ctx.fillStyle = "#c9dcec";
    ctx.font = "10px monospace";
    ctx.fillText("volume", 6, 12);
    ctx.fillText("composite", half + 6, 12);
  }

  function renderDiagnostics() {
    const bits = [lastDiagnostic];
    if (current) {
      if (current.last_compare) {
        bits.push("COMPARE " + (current.last_compare.equal ? "PASS" : "FAIL"));
        bits.push(JSON.stringify(current.last_compare, null, 2));
      }
      if (current.last_truth) {
        bits.push("TRUTH " + (current.last_truth.equal ? "PASS" : "FAIL"));
        bits.push(JSON.stringify(current.last_truth, null, 2));
      }
      if (current.last_report) {
        bits.push("REPORT");
        bits.push(JSON.stringify({
          mode: current.last_report.mode,
          duration_ms: current.last_report.duration_ms,
          requested: current.last_report.requested_blocks,
          computed: current.last_report.computed_blocks,
          reused: current.last_report.reused_blocks,
          batches: current.last_report.batches_committed,
          buffer_waits: current.last_report.buffer_wait_count,
          paused: current.last_report.paused,
          complete: current.last_report.complete,
          quality: current.last_report.quality,
          failures: current.last_report.failures,
        }, null, 2));
      }
    }
    $("diagnostics").textContent = bits.filter(Boolean).join("\n\n");
  }

  function escapeHTML(s) {
    return String(s == null ? "" : s)
      .replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;")
      .replaceAll(">", "&gt;")
      .replaceAll('"', "&quot;");
  }

  function bindCanvasInteractions() {
    $("volumeCanvas").addEventListener("click", (ev) => {
      if (!current) return;
      const rect = ev.currentTarget.getBoundingClientRect();
      const px = (ev.clientX - rect.left) / rect.width * current.image.width;
      const py = (ev.clientY - rect.top) / rect.height * current.image.height;
      selectedBlock = {
        x: Math.max(0, Math.min(current.state.total_blocks ? Math.ceil(current.image.width / current.state.block_size.width) - 1 : 0,
          Math.floor(px / current.state.block_size.width))),
        y: Math.max(0, Math.min(Math.ceil(current.image.height / current.state.block_size.height) - 1,
          Math.floor(py / current.state.block_size.height))),
      };
      loadBlockDetail(selectedBlock.x, selectedBlock.y, false);
      renderAll();
    });
  }

  function bindControls() {
    $("bootBtn").addEventListener("click", async () => {
      await call("Bootstrap", $("scenePath").value.trim(), $("dbPath").value.trim(), 20261124);
    });
    $("fullBtn").addEventListener("click", () => call("RenderFull"));
    $("previewBtn").addEventListener("click", () => call("RenderPreview"));
    $("resumeBtn").addEventListener("click", async () => {
      await call("ResumeRendering");
      await call("RenderResume");
    });
    $("compareBtn").addEventListener("click", () => call("Compare"));
    $("truthBtn").addEventListener("click", () => call("VerifyTruth"));
    $("pauseBtn").addEventListener("click", () => call("Pause"));
    $("retryBtn").addEventListener("click", () => call("RetryFailed"));
    $("applyLightBtn").addEventListener("click", applyLight);
    $("applyMediumBtn").addEventListener("click", applyMedium);
    $("applyConfigBtn").addEventListener("click", applyConfig);
    $("clearCoverageBtn").addEventListener("click", () => {
      selectedLight = "";
      $("lightCoverageHint").textContent = "influence overlay";
      renderAll();
    });
    $("lightSelect").addEventListener("change", () => {
      selectedLight = $("lightSelect").value;
      fillLightEditor();
      renderAll();
    });
    window.addEventListener("resize", () => {
      if (current) renderAll();
    });
  }

  async function boot() {
    bindControls();
    bindCanvasInteractions();
    lastDiagnostic = "loading deterministic scene and checkpoint…";
    renderDiagnostics();
    if (!api()) {
      lastDiagnostic =
        "Wails bindings are not present. This UI is intended to run inside Wails:\n" +
        "  wails dev -tags desktop\n" +
        "or\n" +
        "  wails build -tags desktop";
      renderDiagnostics();
      return;
    }
    try {
      await call("Bootstrap", "", "", 20261124);
    } catch (_) {
      // applyResult already surfaced the diagnostic.
    }
  }

  window.addEventListener("DOMContentLoaded", boot);
})();
