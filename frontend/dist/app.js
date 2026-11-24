(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const state = {
    dash: null,
    autoTimer: null,
    busy: false,
    formsLoaded: false,
    lastSelected: 0,
  };

  const RPC = {
    Bootstrap: { http: "bootstrap" },
    Dashboard: { http: "dashboard" },
    Generate: { http: "generate" },
    UpdateParams: { http: "params" },
    Step: { http: "step" },
    ProcessAll: { http: "process-all" },
    SetPaused: { http: "pause", body: (paused) => ({ paused }) },
    Retry: { http: "retry", body: (window) => ({ window }) },
    SelectWindow: { http: "select", body: (window) => ({ window }) },
    MoveCamera: {
      http: "camera",
      body: (window, dx, dy, dz) => ({ window, dx, dy, dz }),
    },
    Validate: { http: "validate" },
    Reset: { http: "reset" },
    ExportProject: { http: "export" },
    Quit: { http: "quit" },
  };

  function isWails() {
    return !!(window.go && window.go.main && window.go.main.App);
  }

  async function rpc(name, ...args) {
    if (isWails() && typeof window.go.main.App[name] === "function") {
      try {
        return await window.go.main.App[name](...args);
      } catch (err) {
        throw normalizeError(err);
      }
    }
    const spec = RPC[name];
    if (!spec) throw { code: "E_INTERNAL", message: `未知 RPC ${name}` };
    const init = { method: "GET", headers: {} };
    if (args.length > 0) {
      init.method = "POST";
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(spec.body ? spec.body(...args) : args[0]);
    }
    const response = await fetch(`/api/${spec.http}`, init);
    const payload = await response.json().catch(() => null);
    if (!payload) throw { code: "E_IO", message: `HTTP ${response.status}` };
    if (payload.error) throw payload.error;
    return payload.data;
  }

  function normalizeError(err) {
    if (!err) return { code: "E_INTERNAL", message: "未知错误" };
    if (typeof err === "string") {
      const match = err.match(/\b(E_[A-Z_]+)\b/);
      return { code: match ? match[1] : "E_INTERNAL", message: err };
    }
    return {
      code: err.code || err.Code || "E_INTERNAL",
      message: err.message || err.Message || String(err),
      window: err.window || err.Window,
      chunk: err.chunk || err.Chunk,
      field: err.field || err.Field,
      detail: err.detail || err.Detail,
    };
  }

  function errorText(err) {
    const e = normalizeError(err);
    const loc = [];
    if (e.window !== undefined && e.window !== 0) loc.push(`window=${e.window}`);
    if (e.chunk !== undefined && e.chunk !== 0) loc.push(`chunk=${e.chunk}`);
    if (e.field) loc.push(`field=${e.field}`);
    return `${e.code}: ${e.message}${loc.length ? ` [${loc.join(" ")}]` : ""}${e.detail ? ` (${e.detail})` : ""}`;
  }

  function toast(err, ok = false) {
    const node = $("toast");
    node.textContent = ok ? String(err) : errorText(err);
    node.style.borderColor = ok ? "rgba(78,225,162,.5)" : "";
    node.style.background = ok ? "#0f211b" : "";
    node.style.color = ok ? "#c9ffe9" : "";
    node.classList.remove("hidden");
    clearTimeout(toast._timer);
    toast._timer = setTimeout(() => node.classList.add("hidden"), ok ? 2600 : 6000);
  }

  function setBusy(busy) {
    state.busy = busy;
    for (const id of ["btnStep", "btnRun", "btnPause", "btnValidate", "btnValidateInline", "btnGenerate", "btnApply", "btnReset", "btnExport"]) {
      const node = $(id);
      if (node) node.disabled = busy;
    }
  }

  async function invoke(name, ...args) {
    if (state.busy) return null;
    setBusy(true);
    try {
      const result = await rpc(name, ...args);
      return result;
    } catch (err) {
      console.error(err);
      toast(err);
      return null;
    } finally {
      setBusy(false);
    }
  }

  async function refresh(name = "Dashboard", ...args) {
    const dash = await invoke(name, ...args);
    if (!dash) {
      try {
        const recovered = await rpc("Dashboard");
        if (recovered) {
          state.dash = recovered;
          render();
        }
      } catch (_) {}
      return null;
    }
    state.dash = dash;
    if (!state.formsLoaded) loadForms(dash);
    render();
    return dash;
  }

  function loadForms(dash) {
    state.formsLoaded = true;
    const p = dash.params || {};
    const q = p.quantization || dash.project.meta.precision;
    $("seed").value = dash.project.meta.seed;
    $("worldBits").value = q.world_frac_bits;
    $("screenBits").value = q.screen_frac_bits;
    $("genRaster").value = p.raster_resolution || 128;
    $("tolerance").value = (p.tolerance_px_q / (1 << q.screen_frac_bits)).toFixed(3);
    $("raster").value = p.raster_resolution || 128;
    $("workers").value = p.max_workers || 2;
    $("resident").value = p.max_resident || 15;
    $("runtimeTolerance").value = (p.tolerance_px_q / (1 << q.screen_frac_bits)).toFixed(3);
  }

  function bits() {
    return state.dash?.project?.meta?.precision?.screen_frac_bits || 8;
  }

  function px(q, digits = 3) {
    return (Number(q || 0) / (1 << bits())).toFixed(digits);
  }

  function world(q, digits = 1) {
    const wb = state.dash?.project?.meta?.precision?.world_frac_bits || 8;
    return (Number(q || 0) / (1 << wb)).toFixed(digits);
  }

  function shortHash(hash) {
    return hash ? hash.slice(0, 10) : "—";
  }

  function statusColor(status) {
    if (status === "visible") return "#4ee1a2";
    if (status === "uncertain") return "#ffc857";
    return "#5a687c";
  }

  function stateLabel(value) {
    return {
      pending: "待剔除",
      culling: "剔除中",
      loading: "加载中",
      committed: "已提交",
      failed: "失败",
    }[value] || value;
  }

  function render() {
    const d = state.dash;
    if (!d) return;
    renderTop(d);
    renderBanner(d);
    renderKpis(d);
    renderWindows(d);
    renderSelected(d);
    renderSlots(d);
    renderChunkTable(d);
    renderExplainer(d);
    renderPaths(d);
    renderInspector(d);
    drawScene(d);
    drawScreen(d);
    drawErrors(d);
    $("btnPause").disabled = state.busy || d.paused;
    $("btnRun").textContent = state.autoTimer ? "停止自动" : "自动推进";
    $("btnRun").classList.toggle("primary", !state.autoTimer);
  }

  function renderTop(d) {
    $("runtimeBadge").textContent = isWails() ? "WAILS" : "LOCAL HTTP";
    $("projectBadge").textContent = `PROJECT ${shortHash(d.project.content_hash)}`;
    $("progressBadge").textContent = `${d.progress.committed} / ${d.progress.total}`;
    $("seedChip").textContent = `seed ${d.project.meta.seed}`;
  }

  function renderBanner(d) {
    const banner = $("globalBanner");
    const messages = [];
    if (d.snapshot.invalidated) {
      messages.push(`快照已整体作废并全量重放：${d.snapshot.reason || "版本或内容哈希不匹配"}`);
    }
    if (d.invalid_windows && d.invalid_windows.length) {
      messages.push(`${d.invalid_windows.length} 个窗口失效，详见右侧“失效与原因”。`);
    }
    if (!messages.length) {
      banner.classList.add("hidden");
      return;
    }
    banner.textContent = messages.join(" ");
    banner.classList.remove("hidden");
    banner.classList.toggle("error", !!d.invalid_windows?.some((x) => x.kind === "failed"));
  }

  function renderKpis(d) {
    const selected = d.selected;
    $("kpiCommitted").textContent = `${d.progress.committed}/${d.progress.total}`;
    $("kpiProgressText").textContent = d.progress.failed ? `${d.progress.failed} 个窗口待重试` : "窗口顺序稳定";
    $("kpiVisible").textContent = selected ? (selected.visible || []).length : "—";
    $("kpiUncertain").textContent = selected ? (selected.uncertain || []).length : "—";
    $("kpiBound").textContent = selected ? `${px(selected.error.cumulative_upper_bound_q, 2)} px` : "—";
    if (d.verification) {
      const paths = d.verification.three_paths.paths || [];
      const differences = d.verification.three_paths.differences || [];
      $("kpiPaths").textContent = d.verification.passed ? "PASS" : "FAIL";
      $("kpiPaths").style.color = d.verification.passed ? "var(--good)" : "var(--bad)";
      $("kpiPathsDetail").textContent = `${paths.length} 路径 · ${differences.length} 差异`;
    } else {
      $("kpiPaths").textContent = "未验收";
      $("kpiPaths").style.color = "";
      $("kpiPathsDetail").textContent = "运行后比较实时/恢复/重放";
    }
  }

  function renderWindows(d) {
    const host = $("windowMatrix");
    host.innerHTML = "";
    for (const w of d.windows || []) {
      const card = document.createElement("button");
      card.className = `window-card${w.index === d.selected_window ? " selected" : ""}`;
      card.dataset.state = w.state;
      card.dataset.window = w.index;
      card.innerHTML = `
        <div class="wc-top">
          <span class="wc-id">W${String(w.index).padStart(2, "0")}</span>
          <span class="wc-state">${stateLabel(w.state)}</span>
        </div>
        <div class="wc-time">t=${w.time} · offset ${w.camera_offset || "—"}</div>
        <div class="wc-sets">
          <span class="mini-badge good">V ${w.visible?.length || 0}</span>
          <span class="mini-badge warn">U ${w.uncertain?.length || 0}</span>
          <span class="mini-badge">R ${w.resident?.length || 0}</span>
        </div>`;
      host.appendChild(card);
    }
  }

  function renderSelected(d) {
    const s = d.selected;
    const title = $("selectedTitle");
    const stateChip = $("selectedState");
    if (!s) {
      title.textContent = "窗口 —";
      stateChip.textContent = "pending";
      return;
    }
    title.textContent = `窗口 W${String(s.window).padStart(2, "0")}`;
    stateChip.textContent = stateLabel(s.state);
    stateChip.dataset.state = s.state;
    $("selectedChip").textContent = s.frozen ? "FROZEN" : "PREVIEW";
    $("selectedTime").textContent = String(s.time);
    $("selectedOffset").textContent = String(s.camera_offset ?? (s.window + 1));
    $("selectedParams").textContent = `v${s.params_version || 1} · ${shortHash(s.params_hash)}`;
    $("selectedFrozen").textContent = s.frozen ? "是（已提交）" : "否（可重算）";
    $("cameraMoveRow").classList.toggle("hidden", s.frozen || s.state === "culling" || s.state === "loading");
    $("btnRetry").classList.toggle("hidden", s.state !== "failed");
  }

  function renderSlots(d) {
    const s = d.selected;
    const max = d.params.max_resident || 15;
    const resident = new Set(s?.resident || []);
    const loaded = new Set(s?.loaded || []);
    const host = $("slotGrid");
    host.innerHTML = "";
    for (let i = 0; i < max; i++) {
      const chunk = [...resident][i];
      const slot = document.createElement("div");
      slot.className = "slot";
      if (chunk !== undefined) {
        slot.classList.add("occupied");
        if (loaded.has(chunk)) slot.classList.add("loaded");
        slot.textContent = `C${String(chunk).padStart(2, "0")}`;
      } else {
        slot.textContent = String(i + 1).padStart(2, "0");
      }
      host.appendChild(slot);
    }
    $("slotChip").textContent = `${resident.size} / ${max}`;
    $("loadedText").textContent = s?.loaded?.length ? s.loaded.map((x) => `C${String(x).padStart(2, "0")}`).join(", ") : "—";
    $("unloadedText").textContent = s?.unloaded?.length ? s.unloaded.map((x) => `C${String(x).padStart(2, "0")}`).join(", ") : "—";
  }

  function renderChunkTable(d) {
    const host = $("chunkRows");
    host.innerHTML = "";
    const s = d.selected;
    if (!s) return;
    for (const c of s.chunks || []) {
      const tr = document.createElement("tr");
      tr.innerHTML = `
        <td class="mono">C${String(c.chunk).padStart(2, "0")}</td>
        <td><span class="status ${c.status}">${c.status}</span></td>
        <td class="mono">${(c.anchor.x_q / (1 << bits())).toFixed(1)}, ${(c.anchor.y_q / (1 << bits())).toFixed(1)}</td>
        <td class="mono">${c.reference_occluder || "—"}</td>
        <td class="mono">${px(c.error.projection_error_q, 3)}</td>
        <td class="mono">${px(c.error.visibility_upper_bound_q, 2)}</td>`;
      host.appendChild(tr);
    }
  }

  function renderExplainer(d) {
    const s = d.selected;
    const host = $("explainBody");
    if (!s) {
      host.innerHTML = `<p class="muted">选择窗口后显示因误差带而无法二值化判定的分块。</p>`;
      return;
    }
    const uncertain = (s.chunks || []).filter((c) => c.status === "uncertain");
    const cards = uncertain.slice(0, 3).map((c) => `
      <div class="uncertain-card">
        <strong>C${String(c.chunk).padStart(2, "0")} · ${c.reason}</strong>
        <span>参考遮挡体：${c.reference_occluder || "frustum"} · 可见性界 [${(c.visibility_lower_q16 / 65536).toFixed(0)}, ${(c.visibility_upper_q16 / 65536).toFixed(0)}]</span>
        <span>深度界 [${world(c.depth_lower_q)}, ${world(c.depth_upper_q)}] · 覆盖误差 ${px(c.error.occlusion_coverage_error_q, 2)} px</span>
      </div>`).join("");
    host.innerHTML = `
      <p>定点相机变换先产生世界空间残差，再经屏幕除法成为像素残差；遮挡栅格又把像素判定量化到 <b>${px(s.error.coverage_error_q, 2)} px</b> 的误差带。只要真实锚点可能跨过取舍边界，系统就输出不确定，而不是强行二值化。</p>
      <div class="formula">projection = ${px(s.error.projection_error_q, 3)} px<br/>
coverage = ${px(s.error.coverage_error_q, 2)} px · depth = ${world(s.error.depth_compare_error_world_q)} wu<br/>
cumulative upper bound = ${px(s.error.cumulative_upper_bound_q, 2)} px</div>
      ${uncertain.length ? cards : `<p class="muted">当前窗口没有不确定分块；所有判定都落在误差带之外。</p>`}`;
  }

  function renderPaths(d) {
    const report = d.verification;
    const host = $("pathCards");
    const checks = $("checkList");
    if (!report) {
      host.innerHTML = `<div class="empty-state">尚未运行三路径验收。</div>`;
      checks.innerHTML = "";
      return;
    }
    const paths = report.three_paths.paths || [];
    const differences = report.three_paths.differences || [];
    host.innerHTML = paths.map((p) => `
      <article class="path-card ${report.three_paths.equal ? "pass" : "fail"}">
        <div class="pc-name">${p.name}</div>
        <div class="pc-value">${p.committed_count}/${p.window_count}</div>
        <div class="pc-detail">checkpoint ${p.checkpoint_count} · queue ${p.budget_queue_events}<br/>
        ${p.name === "resume" ? `restart@${p.restart_at} · resumed ${p.resumed_from}` : `${p.duration_ms.toFixed(2)} ms`}</div>
      </article>`).join("");
    if (differences.length) {
      host.innerHTML += `<div class="empty-state">${differences.length} 个逐窗差异，验收未通过。</div>`;
    }
    checks.innerHTML = (report.checks || []).map((c) => `
      <div class="check-item ${c.passed ? "" : "fail"}">
        <i></i><span><b>${c.name}</b><br/>${c.detail}</span>
      </div>`).join("");
  }

  function renderInspector(d) {
    $("snapshotCount").textContent = String(d.snapshot.count || 0);
    $("snapshotLast").textContent = d.snapshot.last_window >= 0 ? `W${d.snapshot.last_window}` : "—";
    $("snapshotPath").textContent = d.snapshot.path || "—";
    $("snapshotPath").title = d.snapshot.path || "";

    const invalids = d.invalid_windows || [];
    $("invalidCount").textContent = String(invalids.length);
    $("invalidList").innerHTML = invalids.length
      ? invalids.map((x) => `<div class="invalid-item"><strong>W${String(x.index).padStart(2, "0")} · ${x.kind}</strong><span>${escapeHtml(x.reason)}</span></div>`).join("")
      : `<p class="muted">无失效窗口。</p>`;

    const events = d.budget_events || [];
    $("budgetCount").textContent = String(events.length);
    $("budgetList").innerHTML = events.length
      ? events.slice(-6).reverse().map((e) => `<div class="event-item"><strong>W${e.window} · C${e.chunk} · ${e.reason}</strong><span>${escapeHtml(e.detail)}</span></div>`).join("")
      : `<p class="muted">无阻塞或预算事件。</p>`;

    $("logList").innerHTML = (d.logs || []).slice(-14).reverse().map((l) => `
      <div class="log-item ${l.level}">
        <div class="lt">${new Date(l.at).toLocaleTimeString()} · ${l.level}</div>
        <div class="lm">${escapeHtml(l.message)}</div>
      </div>`).join("") || `<p class="muted">暂无日志。</p>`;
  }

  function escapeHtml(value) {
    return String(value ?? "")
      .replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;")
      .replaceAll(">", "&gt;")
      .replaceAll('"', "&quot;");
  }

  function prepareCanvas(canvas, cssHeight) {
    const rect = canvas.getBoundingClientRect();
    const dpr = Math.max(1, Math.min(2, window.devicePixelRatio || 1));
    const width = Math.max(240, rect.width);
    canvas.width = Math.round(width * dpr);
    canvas.height = Math.round(cssHeight * dpr);
    canvas.style.height = `${cssHeight}px`;
    const ctx = canvas.getContext("2d");
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    return { ctx, w: width, h: cssHeight };
  }

  function drawScene(d) {
    const { ctx, w, h } = prepareCanvas($("sceneCanvas"), 370);
    const bounds = d.project.world.world_bounds;
    const minX = bounds.min.x, maxX = bounds.max.x, minZ = bounds.min.z, maxZ = bounds.max.z;
    const pad = 22;
    const sx = (x) => pad + ((x - minX) / (maxX - minX || 1)) * (w - pad * 2);
    const sy = (z) => h - pad - ((z - minZ) / (maxZ - minZ || 1)) * (h - pad * 2);
    ctx.clearRect(0, 0, w, h);
    ctx.fillStyle = "#0a1019";
    ctx.fillRect(0, 0, w, h);

    ctx.strokeStyle = "rgba(96,215,255,.06)";
    ctx.lineWidth = 1;
    for (let x = -800; x <= 800; x += 200) {
      ctx.beginPath(); ctx.moveTo(sx(x), pad); ctx.lineTo(sx(x), h - pad); ctx.stroke();
    }
    for (let z = -1000; z <= 2000; z += 250) {
      ctx.beginPath(); ctx.moveTo(pad, sy(z)); ctx.lineTo(w - pad, sy(z)); ctx.stroke();
    }

    const statuses = new Map((d.selected?.chunks || []).map((c) => [c.chunk, c]));
    const resident = new Set(d.selected?.resident || []);
    for (const occ of d.project.world.occluders) {
      const x = sx(occ.bounds.min.x), y = sy(occ.bounds.max.z);
      const ww = Math.max(2, sx(occ.bounds.max.x) - sx(occ.bounds.min.x));
      const hh = Math.max(2, sy(occ.bounds.min.z) - sy(occ.bounds.max.z));
      ctx.fillStyle = "rgba(153,168,194,.16)";
      ctx.strokeStyle = "rgba(165,181,207,.45)";
      ctx.fillRect(x, y, ww, hh);
      ctx.strokeRect(x, y, ww, hh);
      ctx.save();
      ctx.beginPath(); ctx.rect(x, y, ww, hh); ctx.clip();
      ctx.strokeStyle = "rgba(165,181,207,.16)";
      for (let line = -hh; line < ww + hh; line += 7) {
        ctx.beginPath(); ctx.moveTo(x + line, y); ctx.lineTo(x + line - hh, y + hh); ctx.stroke();
      }
      ctx.restore();
      ctx.fillStyle = "rgba(210,220,238,.65)";
      ctx.font = "9px " + getComputedStyle(document.body).getPropertyValue("--mono");
      ctx.fillText(occ.id, x + 3, y + 10);
    }

    for (const chunk of d.project.world.chunks) {
      const c = statuses.get(chunk.index);
      const x = sx(chunk.bounds.min.x), y = sy(chunk.bounds.max.z);
      const ww = Math.max(3, sx(chunk.bounds.max.x) - sx(chunk.bounds.min.x));
      const hh = Math.max(3, sy(chunk.bounds.min.z) - sy(chunk.bounds.max.z));
      const color = statusColor(c?.status || "occluded");
      ctx.fillStyle = hexAlpha(color, c ? .22 : .08);
      ctx.strokeStyle = hexAlpha(color, c?.status === "uncertain" ? .95 : .75);
      ctx.lineWidth = resident.has(chunk.index) ? 2.3 : 1.1;
      ctx.fillRect(x, y, ww, hh);
      ctx.strokeRect(x, y, ww, hh);
      if (resident.has(chunk.index)) {
        ctx.strokeStyle = "rgba(255,255,255,.42)";
        ctx.lineWidth = 1;
        ctx.strokeRect(x + 2, y + 2, Math.max(1, ww - 4), Math.max(1, hh - 4));
      }
      ctx.fillStyle = hexAlpha(color, .94);
      ctx.font = "8px " + getComputedStyle(document.body).getPropertyValue("--mono");
      ctx.fillText(`C${String(chunk.index).padStart(2, "0")}`, x + 3, y + hh - 4);
    }

    const cam = d.selected?.camera || d.project.world.cameras[d.selected_window || 0];
    if (cam) {
      const cx = sx(cam.position.x), cz = sy(cam.position.z);
      const fwdX = cam.forward.x, fwdZ = cam.forward.z;
      const yaw = Math.atan2(fwdX, fwdZ);
      const half = Math.atan(cam.tan_half_x / (1 << d.project.meta.precision.param_frac_bits));
      const far = 1450;
      const points = [
        [cam.position.x, cam.position.z],
        [cam.position.x + Math.sin(yaw - half) * far, cam.position.z + Math.cos(yaw - half) * far],
        [cam.position.x + Math.sin(yaw + half) * far, cam.position.z + Math.cos(yaw + half) * far],
      ];
      ctx.beginPath();
      points.forEach(([x, z], i) => i ? ctx.lineTo(sx(x), sy(z)) : ctx.moveTo(sx(x), sy(z)));
      ctx.closePath();
      const grad = ctx.createLinearGradient(cx, cz, sx(points[1][0]), sy(points[1][1]));
      grad.addColorStop(0, "rgba(96,215,255,.18)");
      grad.addColorStop(1, "rgba(96,215,255,0)");
      ctx.fillStyle = grad;
      ctx.fill();
      ctx.strokeStyle = "rgba(96,215,255,.55)";
      ctx.lineWidth = 1.2;
      ctx.stroke();
      ctx.fillStyle = "#60d7ff";
      ctx.beginPath(); ctx.arc(cx, cz, 4, 0, Math.PI * 2); ctx.fill();
      ctx.strokeStyle = "rgba(96,215,255,.4)";
      ctx.beginPath(); ctx.arc(cx, cz, 10, 0, Math.PI * 2); ctx.stroke();
      ctx.fillStyle = "rgba(200,240,255,.9)";
      ctx.font = "9px " + getComputedStyle(document.body).getPropertyValue("--mono");
      ctx.fillText(`CAM W${d.selected_window}`, cx + 9, cz - 8);
    }
  }

  function drawScreen(d) {
    const { ctx, w, h } = prepareCanvas($("screenCanvas"), 370);
    const cam = d.selected?.camera || d.project.world.cameras[d.selected_window || 0];
    if (!cam) return;
    const screenW = cam.screen_width;
    const screenH = cam.screen_height;
    $("renderChip").textContent = `${screenW} × ${screenH}`;
    const pad = 18;
    const scale = Math.min((w - pad * 2) / screenW, (h - pad * 2) / screenH);
    const ox = (w - screenW * scale) / 2;
    const oy = (h - screenH * scale) / 2;
    const sx = (q) => ox + (q / (1 << bits())) * scale;
    const sy = (q) => oy + (q / (1 << bits())) * scale;
    ctx.clearRect(0, 0, w, h);
    ctx.fillStyle = "#080d15";
    ctx.fillRect(0, 0, w, h);
    ctx.fillStyle = "#0b121c";
    ctx.fillRect(ox, oy, screenW * scale, screenH * scale);
    ctx.strokeStyle = "rgba(96,215,255,.22)";
    ctx.strokeRect(ox, oy, screenW * scale, screenH * scale);
    ctx.strokeStyle = "rgba(255,255,255,.035)";
    for (let x = 0; x <= screenW; x += screenW / 8) {
      ctx.beginPath(); ctx.moveTo(ox + x * scale, oy); ctx.lineTo(ox + x * scale, oy + screenH * scale); ctx.stroke();
    }
    for (let y = 0; y <= screenH; y += screenH / 6) {
      ctx.beginPath(); ctx.moveTo(ox, oy + y * scale); ctx.lineTo(ox + screenW * scale, oy + y * scale); ctx.stroke();
    }

    for (const occ of d.project.world.occluders) {
      const rect = projectAABB(cam, occ.bounds, d.project.meta.precision);
      if (!rect) continue;
      const x = sx(rect.minX), y = sy(rect.minY);
      const ww = Math.max(1, sx(rect.maxX) - x);
      const hh = Math.max(1, sy(rect.maxY) - y);
      ctx.fillStyle = "rgba(125,145,175,.14)";
      ctx.strokeStyle = "rgba(165,181,207,.32)";
      ctx.fillRect(x, y, ww, hh);
      ctx.strokeRect(x, y, ww, hh);
      ctx.fillStyle = "rgba(210,220,238,.55)";
      ctx.font = "8px " + getComputedStyle(document.body).getPropertyValue("--mono");
      ctx.fillText(occ.id, x + 2, y + 9);
    }
    for (const c of d.selected?.chunks || []) {
      const b = c.screen_bounds;
      if (!b) continue;
      const x = sx(b.min_x_q), y = sy(b.min_y_q);
      const ww = Math.max(1.5, sx(b.max_x_q) - x);
      const hh = Math.max(1.5, sy(b.max_y_q) - y);
      const color = statusColor(c.status);
      ctx.fillStyle = hexAlpha(color, .12);
      ctx.strokeStyle = hexAlpha(color, .82);
      ctx.lineWidth = 1;
      ctx.fillRect(x, y, ww, hh);
      ctx.strokeRect(x, y, ww, hh);
      ctx.fillStyle = hexAlpha(color, .95);
      ctx.font = "8px " + getComputedStyle(document.body).getPropertyValue("--mono");
      ctx.fillText(`C${String(c.chunk).padStart(2, "0")}`, x + 2, y + 9);
      if (c.status === "uncertain") {
        ctx.strokeStyle = "rgba(255,200,87,.22)";
        ctx.strokeRect(x - 2, y - 2, ww + 4, hh + 4);
      }
    }
    if (d.selected) {
      ctx.fillStyle = "rgba(96,215,255,.85)";
      ctx.font = "9px " + getComputedStyle(document.body).getPropertyValue("--mono");
      ctx.fillText(d.selected.frozen ? "FROZEN FRAME" : "LIVE PREVIEW", ox + 7, oy + 12);
    }
  }

  function projectAABB(cam, box, precision) {
    const wb = precision.world_frac_bits;
    const bb = precision.basis_frac_bits;
    const sb = precision.screen_frac_bits;
    const points = [];
    for (const x of [box.min.x, box.max.x]) {
      for (const y of [box.min.y, box.max.y]) {
        for (const z of [box.min.z, box.max.z]) {
          const rx = (x - cam.position.x) / (1 << wb);
          const ry = (y - cam.position.y) / (1 << wb);
          const rz = (z - cam.position.z) / (1 << wb);
          const bx = cam.right.x / (1 << bb), by = cam.right.y / (1 << bb), bz = cam.right.z / (1 << bb);
          const ux = cam.up.x / (1 << bb), uy = cam.up.y / (1 << bb), uz = cam.up.z / (1 << bb);
          const fx = cam.forward.x / (1 << bb), fy = cam.forward.y / (1 << bb), fz = cam.forward.z / (1 << bb);
          const cx = rx * bx + ry * by + rz * bz;
          const cy = rx * ux + ry * uy + rz * uz;
          const cz = rx * fx + ry * fy + rz * fz;
          if (cz <= cam.near / (1 << wb)) return null;
          const fxq = cam.focal_x / (1 << sb);
          const fyq = cam.focal_y / (1 << sb);
          const cxq = cam.center_x / (1 << sb);
          const cyq = cam.center_y / (1 << sb);
          points.push([cxq + fxq * cx / cz, cyq - fyq * cy / cz]);
        }
      }
    }
    return {
      minX: Math.min(...points.map((p) => p[0])) * (1 << sb),
      maxX: Math.max(...points.map((p) => p[0])) * (1 << sb),
      minY: Math.min(...points.map((p) => p[1])) * (1 << sb),
      maxY: Math.max(...points.map((p) => p[1])) * (1 << sb),
    };
  }

  function drawErrors(d) {
    const { ctx, w, h } = prepareCanvas($("errorCanvas"), 245);
    ctx.clearRect(0, 0, w, h);
    ctx.fillStyle = "#0a1019";
    ctx.fillRect(0, 0, w, h);
    const series = d.error_series || [];
    if (!series.length) {
      ctx.fillStyle = "rgba(138,154,177,.7)";
      ctx.font = "11px " + getComputedStyle(document.body).getPropertyValue("--sans");
      ctx.fillText("提交首个窗口后显示误差传播曲线", 24, h / 2);
      return;
    }
    const pad = { l: 42, r: 16, t: 18, b: 30 };
    const values = series.flatMap((p) => [
      p.projection_error_q / (1 << bits()),
      p.coverage_error_q / (1 << bits()),
      p.cumulative_upper_bound_q / (1 << bits()),
    ]);
    const maxY = Math.max(0.5, ...values) * 1.12;
    const minW = Math.min(...series.map((p) => p.window));
    const maxW = Math.max(...series.map((p) => p.window));
    const sx = (window) => pad.l + ((window - minW) / Math.max(1, maxW - minW)) * (w - pad.l - pad.r);
    const sy = (pxValue) => h - pad.b - (pxValue / maxY) * (h - pad.t - pad.b);
    ctx.strokeStyle = "rgba(255,255,255,.05)";
    ctx.fillStyle = "rgba(138,154,177,.65)";
    ctx.font = "9px " + getComputedStyle(document.body).getPropertyValue("--mono");
    for (let i = 0; i <= 4; i++) {
      const y = pad.t + (i / 4) * (h - pad.t - pad.b);
      const value = maxY * (1 - i / 4);
      ctx.beginPath(); ctx.moveTo(pad.l, y); ctx.lineTo(w - pad.r, y); ctx.stroke();
      ctx.fillText(`${value.toFixed(1)}`, 8, y + 3);
    }
    for (const point of series) {
      ctx.fillStyle = point.committed ? "rgba(138,154,177,.7)" : "rgba(255,200,87,.75)";
      ctx.font = "9px " + getComputedStyle(document.body).getPropertyValue("--mono");
      ctx.fillText(`W${point.window}`, sx(point.window) - 8, h - 10);
    }
    const line = (key, color, width) => {
      ctx.beginPath();
      series.forEach((p, i) => {
        const x = sx(p.window), y = sy(p[key] / (1 << bits()));
        if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
      });
      ctx.strokeStyle = color;
      ctx.lineWidth = width;
      ctx.stroke();
      for (const p of series) {
        ctx.beginPath();
        ctx.fillStyle = color;
        ctx.arc(sx(p.window), sy(p[key] / (1 << bits())), 2.3, 0, Math.PI * 2);
        ctx.fill();
      }
    };
    line("projection_error_q", "#60d7ff", 1.4);
    line("coverage_error_q", "#b48dff", 1.2);
    line("cumulative_upper_bound_q", "#ff6b7a", 1.7);
    if (series.length === 1) {
      ctx.fillStyle = "rgba(255,200,87,.7)";
      ctx.font = "9px " + getComputedStyle(document.body).getPropertyValue("--sans");
      ctx.fillText("单个采样点", sx(series[0].window) + 8, sy(series[0].cumulative_upper_bound_q / (1 << bits())) - 7);
    }
  }

  function hexAlpha(hex, alpha) {
    const raw = hex.replace("#", "");
    const r = parseInt(raw.slice(0, 2), 16);
    const g = parseInt(raw.slice(2, 4), 16);
    const b = parseInt(raw.slice(4, 6), 16);
    return `rgba(${r},${g},${b},${alpha})`;
  }

  async function doStep() {
    const dash = await refresh("Step");
    if (dash && !dash.can_step) stopAuto();
  }

  function startAuto() {
    stopAuto();
    state.autoTimer = setInterval(() => doStep(), 620);
    render();
  }

  function stopAuto() {
    if (state.autoTimer) clearInterval(state.autoTimer);
    state.autoTimer = null;
    if (state.dash) render();
  }

  function bind() {
    $("btnStep").addEventListener("click", () => doStep());
    $("btnRun").addEventListener("click", async () => {
      if (state.autoTimer) {
        stopAuto();
        return;
      }
      if (state.dash?.paused) {
        const dash = await invoke("SetPaused", false);
        if (dash) {
          state.dash = dash;
          render();
        }
      }
      startAuto();
    });
    $("btnPause").addEventListener("click", async () => {
      stopAuto();
      const dash = await invoke("SetPaused", true);
      if (dash) {
        state.dash = dash;
        render();
      }
    });
    $("btnValidate").addEventListener("click", runValidation);
    $("btnValidateInline").addEventListener("click", runValidation);
    $("btnGenerate").addEventListener("click", async () => {
      const opts = {
        seed: Number($("seed").value),
        world_frac_bits: Number($("worldBits").value),
        basis_frac_bits: 16,
        screen_frac_bits: Number($("screenBits").value),
        param_frac_bits: 16,
        raster_resolution: Number($("genRaster").value),
        max_workers: Number($("workers").value),
        max_resident: Number($("resident").value),
        tolerance_px: Number($("tolerance").value),
      };
      stopAuto();
      const dash = await invoke("Generate", opts);
      if (dash) {
        state.dash = dash;
        state.formsLoaded = false;
        loadForms(dash);
        render();
        toast("场景工程已按固定种子重新生成", true);
      }
    });
    $("btnApply").addEventListener("click", async () => {
      const opts = {
        raster_resolution: Number($("raster").value),
        max_workers: Number($("workers").value),
        raster_buffer_cells: Math.max(128 * 128, Number($("raster").value) ** 2),
        max_resident: Number($("resident").value),
        tolerance_px: Number($("runtimeTolerance").value),
      };
      const dash = await invoke("UpdateParams", opts);
      if (dash) {
        state.dash = dash;
        render();
        toast("参数已应用到未提交窗口", true);
      }
    });
    $("btnReset").addEventListener("click", async () => {
      stopAuto();
      const dash = await invoke("Reset");
      if (dash) {
        state.dash = dash;
        render();
        toast("检查点已清空", true);
      }
    });
    $("btnExport").addEventListener("click", async () => {
      const path = await invoke("ExportProject");
      if (path) toast(`工程已导出：${path}`, true);
    });
    $("btnRetry").addEventListener("click", async () => {
      if (!state.dash?.selected) return;
      const dash = await invoke("Retry", state.dash.selected.window);
      if (dash) {
        state.dash = dash;
        render();
      }
    });
    $("cameraMoveRow").addEventListener("click", async (event) => {
      const button = event.target.closest("[data-camera-dx]");
      if (!button || !state.dash?.selected) return;
      const dash = await invoke(
        "MoveCamera",
        state.dash.selected.window,
        Number(button.dataset.cameraDx),
        0,
        0,
      );
      if (dash) {
        state.dash = dash;
        render();
        toast("相机位置已调整；仅影响未提交窗口", true);
      }
    });
    $("btnQuit").addEventListener("click", async () => {
      if (isWails()) {
        try { await window.go.main.App.Quit(); } catch (_) {}
      } else {
        window.close();
      }
    });
    $("windowMatrix").addEventListener("click", async (event) => {
      const card = event.target.closest("[data-window]");
      if (!card) return;
      const dash = await invoke("SelectWindow", Number(card.dataset.window));
      if (dash) {
        state.dash = dash;
        state.lastSelected = dash.selected_window;
        render();
      }
    });
    window.addEventListener("resize", () => {
      if (state.dash) {
        drawScene(state.dash);
        drawScreen(state.dash);
        drawErrors(state.dash);
      }
    });
    document.addEventListener("keydown", (event) => {
      if (event.key === "Escape") $("toast").classList.add("hidden");
      if (event.key === " " && event.target === document.body) {
        event.preventDefault();
        doStep();
      }
    });
  }

  async function runValidation() {
    stopAuto();
    const report = await invoke("Validate");
    if (!report) return;
    state.dash.verification = report;
    const dash = await rpc("Dashboard");
    if (dash) state.dash = dash;
    render();
    toast(report.passed ? "三路径、投影与误差界验收通过" : "验收未通过，请查看检查项", report.passed);
  }

  async function boot() {
    bind();
    try {
      const dash = await rpc("Bootstrap");
      state.dash = dash;
      loadForms(dash);
      render();
    } catch (err) {
      console.error(err);
      toast(err);
      $("globalBanner").textContent = errorText(err);
      $("globalBanner").classList.remove("hidden");
      $("globalBanner").classList.add("error");
    }
  }

  boot();
})();
