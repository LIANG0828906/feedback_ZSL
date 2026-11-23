(() => {
  "use strict";

  const state = {
    bootstrap: null,
    samples: [],
    sample: null,
    view: null,
    jobId: "",
    image: null,
    imageSrc: "",
    truth: null,
    selectedLine: -1,
    tab: "lines",
    comparison: null,
    polling: null,
    busy: false,
    desktop: false,
    latestSummary: null,
  };

  const $ = (id) => document.getElementById(id);
  const qsa = (sel) => Array.from(document.querySelectorAll(sel));

  function isWails() {
    return !!(window.go && window.go.main && window.go.main.App && window.go.main.App.Call);
  }

  async function call(method, payload = {}) {
    let envelope;
    if (isWails()) {
      const raw = await window.go.main.App.Call(method, JSON.stringify(payload));
      envelope = JSON.parse(raw);
    } else {
      const response = await fetch("/api/call", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ method, payload }),
      });
      envelope = await response.json();
    }
    if (!envelope.ok) {
      throw envelope.error || { code: "E_STORE", message: "Unknown bridge error" };
    }
    return envelope.data;
  }

  function setBusy(value) {
    state.busy = value;
    qsa("button").forEach((button) => {
      if (!button.dataset.always) button.disabled = value;
    });
  }

  function toast(message, type = "") {
    const el = document.createElement("div");
    el.className = "toast " + type;
    el.textContent = message;
    $("toast-stack").appendChild(el);
    setTimeout(() => el.remove(), 4200);
  }

  function showError(error) {
    const code = error && error.code ? error.code : "E_STORE";
    const message = error && error.message ? error.message : String(error);
    const context = [];
    if (error && error.field) context.push("field=" + error.field);
    if (error && error.expected) context.push("expected=" + error.expected);
    if (error && error.actual) context.push("actual=" + error.actual);
    if (error && Number.isInteger(error.tile_index) && error.tile_index >= 0) context.push("tile=" + error.tile_index + " @" + error.tile_x + "," + error.tile_y);
    if (error && Number.isInteger(error.line_index) && error.line_index >= 0) context.push("line=" + error.line_index);
    if (error && error.break_x) context.push("break=" + error.break_x + "," + error.break_y);
    toast(code + " · " + message + (context.length ? " · " + context.join(" ") : ""), "error");
  }

  function modal(title, body, eyebrow = "PIPELINE EVENT") {
    $("modal-eyebrow").textContent = eyebrow;
    $("modal-title").textContent = title;
    $("modal-body").textContent = typeof body === "string" ? body : JSON.stringify(body, null, 2);
    $("modal").hidden = false;
  }

  function formatHash(hash) {
    if (!hash) return "—";
    return hash.length > 14 ? hash.slice(0, 12) + "…" : hash;
  }

  function fmt(value, digits = 0) {
    if (value === null || value === undefined || Number.isNaN(Number(value))) return "—";
    return Number(value).toFixed(digits);
  }

  function paramsFromForm() {
    const base = state.bootstrap ? state.bootstrap.defaults : {};
    const split = base.split || {};
    return {
      tile_size: Number($("p-tile").value),
      band_factor: Number($("p-band-factor").value),
      min_band: base.min_band ?? 4,
      merge_iou: Number($("p-merge-iou").value),
      stroke_consistency: Number($("p-stroke").value),
      min_candidates: Number($("p-min-candidates").value),
      confidence_threshold: Number($("p-confidence").value),
      detector_min_area: Number($("p-min-area").value),
      count_tolerance: Number($("p-tolerance").value),
      template_version: state.bootstrap ? state.bootstrap.template_version : "bitmap5x7-v1",
      split: {
        ...split,
        template_width: Number($("p-template-w").value),
        template_height: Number($("p-template-h").value),
        deskew_max_deg: Number($("p-deskew").value),
        deskew_step_deg: Number($("p-deskew-step").value),
      },
    };
  }

  function writeParams(params) {
    if (!params) return;
    $("p-tile").value = params.tile_size;
    $("p-band-factor").value = params.band_factor;
    $("p-merge-iou").value = params.merge_iou;
    $("p-stroke").value = params.stroke_consistency;
    $("p-min-candidates").value = params.min_candidates;
    $("p-confidence").value = params.confidence_threshold;
    $("p-tolerance").value = params.count_tolerance;
    $("p-min-area").value = params.detector_min_area;
    const split = params.split || {};
    $("p-template-w").value = split.template_width ?? 24;
    $("p-template-h").value = split.template_height ?? 32;
    $("p-deskew").value = split.deskew_max_deg ?? 10;
    $("p-deskew-step").value = split.deskew_step_deg ?? 0.25;
  }

  async function bootstrap() {
    try {
      setBusy(true);
      const data = await call("bootstrap");
      state.bootstrap = data;
      state.samples = data.manifest.samples || [];
      state.desktop = !!data.desktop;
      $("runtime-mode").textContent = isWails() ? "WAILS DESKTOP" : "LOCAL HTTP LAB";
      $("algorithm-chip").textContent = data.algorithm_version;
      $("template-chip").textContent = data.template_version;
      $("seed-line").textContent = "seed " + data.manifest.seed + " · font " + data.manifest.font_version + " · network " + data.manifest.network_used;
      $("sample-count").textContent = state.samples.length;
      writeParams(data.defaults);
      renderSamples();
      if (state.samples.length) {
        await selectSample(state.samples[0].id);
      }
      if (data.latest_run) {
        state.latestSummary = data.latest_run;
      }
    } catch (error) {
      showError(error);
    } finally {
      setBusy(false);
    }
  }

  function renderSamples() {
    const list = $("sample-list");
    list.innerHTML = "";
    state.samples.forEach((sample) => {
      const button = document.createElement("button");
      button.className = "sample-item" + (state.sample && state.sample.id === sample.id ? " active" : "");
      button.innerHTML = `
        <div class="sample-name"><span>${escapeHTML(sample.title)}</span><em>${sample.channels}ch</em></div>
        <div class="sample-meta"><span>${sample.width}×${sample.height}</span><span>${sample.line_count} lines</span><span>${sample.rotation_degree.toFixed(1)}°</span></div>
      `;
      button.addEventListener("click", () => selectSample(sample.id));
      list.appendChild(button);
    });
  }

  async function selectSample(sampleId) {
    const sample = state.samples.find((item) => item.id === sampleId);
    if (!sample) return;
    state.sample = sample;
    state.selectedLine = -1;
    state.comparison = null;
    state.jobId = "";
    stopPolling();
    renderSamples();
    $("active-title").textContent = sample.title;
    $("active-sub").textContent = sample.id + " · " + sample.image;
    $("hud-top").textContent = sample.width + " × " + sample.height + " · " + sample.channels + " CH";
    $("hud-bottom").textContent = "LOADING FRAME";
    setBusy(true);
    try {
      const [image, truth, latest] = await Promise.all([
        call("sample.image", { sample_id: sampleId }),
        call("sample.truth", { sample_id: sampleId }),
        call("run.latest_view", { sample_id: sampleId }),
      ]);
      state.truth = truth;
      await loadImage(image.data_url);
      state.view = latest.found ? latest.run : null;
      if (state.view) {
        writeParams(state.view.params);
      } else {
        writeParams(state.bootstrap.defaults);
      }
      renderAll();
    } catch (error) {
      showError(error);
    } finally {
      setBusy(false);
    }
  }

  function loadImage(src) {
    return new Promise((resolve, reject) => {
      const image = new Image();
      image.onload = () => {
        state.image = image;
        state.imageSrc = src;
        $("canvas-empty").style.display = "none";
        drawCanvas();
        resolve();
      };
      image.onerror = () => reject({ code: "E_DECODE", message: "Image failed to load" });
      image.src = src;
    });
  }

  function setView(view) {
    state.view = view;
    renderAll();
  }

  function renderAll() {
    const view = state.view;
    if (view) {
      $("active-title").textContent = view.sample_title || (state.sample && state.sample.title) || "—";
      $("active-sub").textContent = (view.sample_id || "—") + " · " + (view.run_id ? "run " + view.run_id : "pending") + " · " + (view.status || "—").toUpperCase();
      $("hud-bottom").textContent = (view.status || "IDLE").toUpperCase() + " · band " + (view.band_width || "—");
    }
    renderMetrics();
    renderLines();
    renderTiles();
    renderCommits();
    renderErrors();
    renderInspector();
    renderComparison();
    updateControls();
    drawCanvas();
  }

  function renderMetrics() {
    const view = state.view;
    const stats = view ? view.stats : null;
    if (!stats) {
      $("m-tiles").textContent = "0 / 0";
      $("m-lines").textContent = "0 / 0";
      $("m-conf").textContent = "—";
      $("m-accuracy").textContent = "—";
      $("progress-fill").style.width = "0%";
      $("progress-text").textContent = "等待样本";
      return;
    }
    $("m-tiles").textContent = stats.committed_tiles + " / " + stats.tile_count;
    $("m-candidates").textContent = stats.candidate_count + " candidates";
    $("m-lines").textContent = stats.committed_lines + " / " + stats.detected_lines;
    $("m-chars").textContent = stats.char_count + " chars · " + stats.empty_lines + " empty";
    $("m-conf").textContent = stats.committed_lines ? stats.avg_confidence.toFixed(3) : "—";
    $("m-minconf").textContent = "min " + (stats.committed_lines ? stats.min_confidence.toFixed(3) : "—") + " · " + stats.low_confidence_lines + " review";
    $("m-accuracy").textContent = stats.text_accuracy ? (stats.text_accuracy * 100).toFixed(1) + "%" : "—";
    $("m-iou").textContent = "IoU " + (stats.avg_truth_iou || 0).toFixed(3) + " · Δ" + stats.count_deviation;
    const total = (stats.tile_count || 0) + (stats.detected_lines || 0);
    const done = (stats.committed_tiles || 0) + (stats.committed_lines || 0);
    $("progress-fill").style.width = total ? Math.min(100, done / total * 100) + "%" : "0%";
    $("progress-text").textContent = view.progress_text || (done + " / " + total);
    $("tab-lines-count").textContent = view.lines.length;
    $("tab-tiles-count").textContent = view.tiles.length;
    $("tab-commits-count").textContent = view.commits.length;
  }

  function renderLines() {
    const body = $("lines-body");
    body.innerHTML = "";
    const lines = state.view ? state.view.lines || [] : [];
    if (!lines.length) {
      body.innerHTML = '<div class="empty-row">尚无检测文本行</div>';
      return;
    }
    lines.forEach((line, index) => {
      const row = document.createElement("div");
      row.className = "line-row" + (state.selectedLine === index ? " selected" : "");
      const confidence = Number(line.confidence || 0);
      const confClass = confidence < 0.55 ? "bad" : confidence < 0.7 ? "low" : "";
      row.innerHTML = `
        <span class="line-index">${String(line.reading_index).padStart(2, "0")}</span>
        <span class="state-badge ${line.state}">${line.state}</span>
        <span class="line-text">${escapeHTML(line.text || (line.empty ? "∅ EMPTY LINE" : "—"))}</span>
        <span class="conf-cell ${confClass}"><span class="conf-bar"><i style="width:${Math.max(0, Math.min(1, confidence)) * 100}%"></i></span><span>${confidence.toFixed(3)}</span></span>
        <span class="mono">${(line.characters || []).length}</span>
        <span class="line-seq">#${line.commit_seq || "—"}</span>
      `;
      row.addEventListener("click", () => {
        state.selectedLine = index;
        renderLines();
        renderInspector();
        drawCanvas();
      });
      body.appendChild(row);
    });
  }

  function renderTiles() {
    const matrix = $("tile-matrix");
    matrix.innerHTML = "";
    const tiles = state.view ? state.view.tiles || [] : [];
    if (!tiles.length) {
      matrix.innerHTML = '<div class="empty-row">尚无瓦片</div>';
      return;
    }
    tiles.forEach((tile) => {
      const cell = document.createElement("div");
      cell.className = "tile-cell " + tile.state;
      cell.title = `tile ${tile.index} · ${tile.x},${tile.y} ${tile.w}×${tile.h} · core ${tile.core_w}×${tile.core_h}`;
      cell.innerHTML = `<em>${tile.index}</em><span>${tile.x},${tile.y}</span><strong>${tile.candidate_count}</strong>`;
      cell.addEventListener("click", () => modal("Tile " + tile.index, {
        geometry: { x: tile.x, y: tile.y, w: tile.w, h: tile.h },
        core: { x: tile.core_x, y: tile.core_y, w: tile.core_w, h: tile.core_h },
        state: tile.state,
        candidate_count: tile.candidate_count,
        commit_seq: tile.commit_seq,
        payload_hash: tile.payload_hash,
        error: tile.error,
        candidates: tile.candidates,
      }, "TILE INSPECTION"));
      matrix.appendChild(cell);
    });
  }

  function renderCommits() {
    const body = $("commits-body");
    body.innerHTML = "";
    const commits = state.view ? state.view.commits || [] : [];
    if (!commits.length) {
      body.innerHTML = '<div class="empty-row">尚无提交记录</div>';
      return;
    }
    commits.forEach((commit) => {
      const row = document.createElement("div");
      row.className = "commit-row";
      row.innerHTML = `<span class="seq">${String(commit.seq).padStart(3, "0")}</span><span class="kind">${commit.kind}</span><span>${escapeHTML(commit.unit_key)}</span><span class="hash" title="${commit.payload_hash}">${commit.payload_hash}</span>`;
      row.addEventListener("click", () => modal("Commit #" + commit.seq, commit, "ATOMIC COMMIT AUDIT"));
      body.appendChild(row);
    });
  }

  function renderErrors() {
    const body = $("errors-body");
    const errors = [];
    if (state.view && state.view.error) errors.push({ unit: "run", ...state.view.error });
    if (state.view) {
      (state.view.tiles || []).forEach((tile) => { if (tile.error) errors.push({ unit: "tile " + tile.index, ...tile.error }); });
      (state.view.lines || []).forEach((line) => { if (line.error) errors.push({ unit: "line " + line.reading_index, ...line.error }); });
    }
    $("tab-errors-count").textContent = errors.length;
    body.innerHTML = "";
    if (!errors.length) {
      body.className = "errors-empty";
      body.textContent = "当前没有异常。";
      return;
    }
    body.className = "";
    errors.forEach((error) => {
      const card = document.createElement("div");
      card.className = "error-card";
      const where = [];
      if (error.field) where.push("field " + error.field);
      if (error.expected) where.push("expected " + error.expected);
      if (error.actual) where.push("actual " + error.actual);
      if (error.tile_index >= 0 && error.tile_index !== undefined) where.push("tile " + error.tile_index + " @ " + error.tile_x + "," + error.tile_y);
      if (error.line_index >= 0 && error.line_index !== undefined) where.push("line " + error.line_index);
      if (error.break_x) where.push("break " + error.break_x + "," + error.break_y);
      card.innerHTML = `<div class="code">${escapeHTML(error.code || "E_STORE")} · ${escapeHTML(error.unit || "unit")}</div><div class="msg">${escapeHTML(error.message || "")}</div><div class="where">${escapeHTML(where.join(" · ") || "no structured context")}</div>`;
      body.appendChild(card);
    });
  }

  function renderInspector() {
    const line = state.view && state.selectedLine >= 0 ? state.view.lines[state.selectedLine] : null;
    $("inspector-title").textContent = line ? "Line " + String(line.reading_index).padStart(2, "0") : "等待选择";
    $("inspector-state").className = "state-pill " + (line ? line.state : "");
    $("inspector-state").textContent = line ? line.state : "—";
    if (!line) {
      $("inspector-text").textContent = "—";
      $("inspector-conf").textContent = "—";
      $("char-list").innerHTML = '<span class="muted">选择文本行后显示</span>';
      $("line-kv").innerHTML = "";
      $("line-json").textContent = "—";
      $("truth-kv").innerHTML = "";
      $("unit-error-card").hidden = true;
      return;
    }
    $("inspector-text").textContent = line.text || (line.empty ? "∅ EMPTY LINE" : "—");
    $("inspector-conf").textContent = Number(line.confidence || 0).toFixed(4);
    $("inspector-conf").style.color = line.confidence >= 0.7 ? "var(--green)" : line.confidence >= 0.55 ? "var(--amber)" : "var(--red)";
    const chars = line.characters || [];
    $("char-list").innerHTML = chars.length ? chars.map((char) => {
      const cls = char.confidence < 0.55 ? "bad" : char.confidence < 0.7 ? "warn" : "";
      return `<div class="char-chip ${cls}" title="runner-up ${escapeHTML(char.runner_up || "—")} ${fmt(char.runner_up_score, 4)}"><b>${escapeHTML(char.rune || "?")}</b><small>${Number(char.confidence).toFixed(3)}</small></div>`;
    }).join("") : '<span class="muted">空行 · 无字符匹配</span>';
    $("line-kv").innerHTML = `
      <dt>Reading index</dt><dd>${line.reading_index}</dd>
      <dt>Bounding box</dt><dd>${line.bbox.x},${line.bbox.y} · ${line.bbox.w}×${line.bbox.h}</dd>
      <dt>Accepted</dt><dd>${line.accepted ? "yes" : "review"}</dd>
      <dt>State</dt><dd>${line.state}</dd>
      <dt>Commit seq</dt><dd>${line.commit_seq || "—"}</dd>
      <dt>Payload hash</dt><dd title="${line.payload_hash || ""}">${formatHash(line.payload_hash)}</dd>
      <dt>Tuning</dt><dd>${line.snapshot ? formatHash(line.snapshot.tuning_version) : "—"}</dd>
    `;
    $("line-json").textContent = line.snapshot ? JSON.stringify(line.snapshot, null, 2) : JSON.stringify(line, null, 2);
    const truth = state.truth && state.truth.regions ? state.truth.regions.find((item) => item.line_index === line.reading_index) : null;
    if (truth) {
      const exact = truth.text === line.text;
      $("truth-kv").innerHTML = `
        <dt>Text</dt><dd style="color:${exact ? "var(--green)" : "var(--amber)"}">${escapeHTML(truth.text)}</dd>
        <dt>Characters</dt><dd>${truth.char_count}</dd>
        <dt>Stroke width</dt><dd>${truth.avg_stroke_width}</dd>
        <dt>Quad</dt><dd>${truth.quad.map((point) => point.x + "," + point.y).join(" · ")}</dd>
        <dt>Exact match</dt><dd>${exact ? "true" : "false"}</dd>
      `;
    } else {
      $("truth-kv").innerHTML = '<dt>Truth</dt><dd>not available</dd>';
    }
    $("unit-error-card").hidden = !line.error;
    $("unit-error-body").textContent = line.error ? JSON.stringify(line.error, null, 2) : "";
  }

  function updateControls() {
    const status = state.view ? state.view.status : "";
    const running = status === "running" || status === "starting";
    $("btn-run").disabled = state.busy || running;
    $("btn-pause").disabled = state.busy || !running;
    $("btn-recompute").disabled = state.busy || running;
    $("btn-resume").disabled = state.busy || !(status === "paused" || status === "failed" || status === "interrupted");
    $("btn-resume").textContent = status === "failed" ? "重试失败单元" : "恢复";
    $("btn-cancel").disabled = state.busy || !running;
    $("btn-tune").disabled = state.busy || !state.jobId;
    $("btn-export").disabled = state.busy || !state.view;
    $("btn-compare").disabled = state.busy || running || !state.sample;
  }

  async function runPipeline(mode) {
    if (!state.sample) return;
    setBusy(true);
    try {
      const handle = await call("run.start", {
        sample_id: state.sample.id,
        params: paramsFromForm(),
        mode,
      });
      state.jobId = handle.job_id;
      toast("Pipeline " + mode + " started · job " + handle.job_id.slice(-12), "ok");
      await pollOnce();
      setBusy(false);
      startPolling();
    } catch (error) {
      showError(error);
      setBusy(false);
    }
  }

  async function pollOnce() {
    if (!state.jobId) {
      const latest = await call("run.latest_view", { sample_id: state.sample.id });
      if (latest.found) setView(latest.run);
      return;
    }
    const view = await call("run.snapshot", { job_id: state.jobId });
    setView(view);
    if (["completed", "failed", "cancelled", "paused", "interrupted"].includes(view.status)) {
      stopPolling();
      setBusy(false);
    }
    if (view.error) showError(view.error);
  }

  function startPolling() {
    stopPolling();
    state.polling = setInterval(async () => {
      try {
        await pollOnce();
      } catch (error) {
        stopPolling();
        setBusy(false);
        showError(error);
      }
    }, 350);
  }

  function stopPolling() {
    if (state.polling) {
      clearInterval(state.polling);
      state.polling = null;
    }
  }

  async function pauseRun() {
    if (!state.jobId) return;
    try {
      await call("run.pause", { job_id: state.jobId });
      toast("Pause requested — engine will stop at the next committed unit boundary");
      startPolling();
    } catch (error) {
      showError(error);
    }
  }

  async function resumeRun() {
    if (!state.sample) return;
    setBusy(true);
    try {
      if (state.jobId) {
        const handle = await call("run.resume", { job_id: state.jobId });
        state.jobId = handle.job_id;
      } else {
        const handle = await call("run.start", {
          sample_id: state.sample.id,
          params: paramsFromForm(),
          mode: "resume",
        });
        state.jobId = handle.job_id;
      }
      setBusy(false);
      startPolling();
    } catch (error) {
      showError(error);
      setBusy(false);
    }
  }

  async function cancelRun() {
    if (!state.jobId) return;
    try {
      await call("run.cancel", { job_id: state.jobId });
      toast("Cancel requested — committed units are retained");
      startPolling();
    } catch (error) {
      showError(error);
    }
  }

  async function tuneRun() {
    if (!state.jobId) return;
    try {
      await call("run.tune", { job_id: state.jobId, params: paramsFromForm() });
      toast("Tuning applied to uncommitted units only", "ok");
      await pollOnce();
    } catch (error) {
      showError(error);
    }
  }

  async function compareRun() {
    if (!state.sample) return;
    setBusy(true);
    $("compare-body").innerHTML = '<div class="compare-empty"><div class="compare-orbit"><span></span><span></span><span></span></div><strong>REPLAYING THREE PATHS</strong><p>正在独立 SQLite 数据库中执行实时、重启恢复与全量重算…</p></div>';
    state.comparison = null;
    try {
      const result = await call("run.compare", {
        sample_id: state.sample.id,
        params: paramsFromForm(),
        mode: "reset",
      });
      state.comparison = result;
      switchTab("compare");
      renderComparison();
      toast("Three-path comparison complete · " + (result.equal ? "MATCH" : "DIFF"), result.equal ? "ok" : "error");
    } catch (error) {
      state.comparison = { equal: false, first_diff: error, paths: [], message: error.message || "comparison failed" };
      switchTab("compare");
      renderComparison();
      showError(error);
    } finally {
      setBusy(false);
    }
  }

  function renderComparison() {
    const body = $("compare-body");
    const result = state.comparison;
    if (!result) {
      if (body.querySelector(".compare-result")) {
        body.innerHTML = '<div class="compare-empty"><div class="compare-orbit"><span></span><span></span><span></span></div><strong>REPLAY CONSISTENCY HARNESS</strong><p>实时处理、暂停后重启恢复、全量重算将在独立 SQLite 库中重放，并逐项比较瓦片、文本行、逐字符置信度与提交序列。</p></div>';
      }
      return;
    }
    const paths = result.paths || [];
    const cards = paths.map((path) => `
      <div class="compare-card">
        <div class="path-name">${escapeHTML(path.name.toUpperCase())}</div>
        <strong>${escapeHTML(path.status || "failed")}</strong>
        <small>commits ${path.commit_count || 0}</small>
        <small>accuracy ${path.stats ? (path.stats.text_accuracy * 100).toFixed(1) + "%" : "—"}</small>
        <small>confidence ${path.stats ? Number(path.stats.avg_confidence || 0).toFixed(3) : "—"}</small>
        <small>${path.duration_ms || 0} ms</small>
        ${path.error ? `<small style="color:var(--red)">${escapeHTML(path.error.code || "error")}</small>` : ""}
      </div>
    `).join("");
    const diff = result.first_diff;
    const diffBox = diff ? `<div class="diff-box">FIRST DIFF · ${escapeHTML(diff.code || "E_DIFF")} · ${escapeHTML(diff.difference_field || "—")}<br>${escapeHTML(diff.message || "")}<br>${diff.tile_index >= 0 ? "tile " + diff.tile_index + " @ " + diff.tile_x + "," + diff.tile_y : ""}${diff.line_index >= 0 ? " · line " + diff.line_index : ""}</div>` : "";
    body.className = "compare-result";
    body.innerHTML = `
      <div class="compare-banner ${result.equal ? "" : "bad"}">
        <div><strong>${result.equal ? "IDENTICAL REPLAY" : "REPLAY DIVERGENCE"}</strong><div class="muted">${escapeHTML(result.message || "")}</div></div>
        <span>lines ${result.compared_lines || 0} · commits ${result.compared_commits || 0}</span>
      </div>
      <div class="compare-cards">${cards}</div>
      ${diffBox}
    `;
  }

  function switchTab(name) {
    state.tab = name;
    qsa(".tab").forEach((tab) => tab.classList.toggle("active", tab.dataset.tab === name));
    qsa(".tab-pane").forEach((pane) => pane.classList.toggle("active", pane.id === "pane-" + name));
  }

  function drawCanvas() {
    const canvas = $("viewport");
    const wrap = $("canvas-wrap");
    const rect = wrap.getBoundingClientRect();
    if (!rect.width || !rect.height) return;
    const dpr = Math.min(2, window.devicePixelRatio || 1);
    canvas.width = Math.max(1, Math.floor(rect.width * dpr));
    canvas.height = Math.max(1, Math.floor(rect.height * dpr));
    const ctx = canvas.getContext("2d");
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, rect.width, rect.height);
    if (!state.image) return;

    const image = state.image;
    const scale = Math.min(rect.width / image.width, rect.height / image.height);
    const drawW = image.width * scale;
    const drawH = image.height * scale;
    const offsetX = (rect.width - drawW) / 2;
    const offsetY = (rect.height - drawH) / 2;
    ctx.save();
    ctx.shadowColor = "rgba(0,0,0,.65)";
    ctx.shadowBlur = 22;
    ctx.drawImage(image, offsetX, offsetY, drawW, drawH);
    ctx.restore();
    const toX = (x) => offsetX + (x / image.width) * drawW;
    const toY = (y) => offsetY + (y / image.height) * drawH;
    const scaled = (v) => (v / image.width) * drawW;
    const scaledY = (v) => (v / image.height) * drawH;
    const view = state.view;
    if (!view) return;

    if ($("ov-tiles").checked) {
      (view.tiles || []).forEach((tile) => {
        const color = tile.state === "committed" ? "rgba(127,242,168,.25)" : tile.state === "failed" ? "rgba(255,111,125,.45)" : tile.state === "detecting" ? "rgba(100,232,225,.55)" : "rgba(124,168,255,.18)";
        ctx.strokeStyle = color;
        ctx.lineWidth = tile.state === "detecting" ? 1.5 : 0.7;
        ctx.strokeRect(toX(tile.x), toY(tile.y), scaled(tile.w), scaledY(tile.h));
        ctx.strokeStyle = color.replace(/[\d.]+\)$/, "0.1)");
        ctx.setLineDash([3, 4]);
        ctx.strokeRect(toX(tile.core_x), toY(tile.core_y), scaled(tile.core_w), scaledY(tile.core_h));
        ctx.setLineDash([]);
      });
    }
    if ($("ov-candidates").checked) {
      ctx.strokeStyle = "rgba(100,232,225,.36)";
      ctx.lineWidth = 0.65;
      (view.tiles || []).forEach((tile) => {
        (tile.candidates || []).forEach((candidate) => {
          ctx.strokeRect(toX(candidate.bbox.x), toY(candidate.bbox.y), scaled(candidate.bbox.w), scaledY(candidate.bbox.h));
        });
      });
    }
    if ($("ov-truth").checked && state.truth) {
      ctx.strokeStyle = "rgba(182,150,255,.55)";
      ctx.lineWidth = 1;
      ctx.setLineDash([5, 4]);
      (state.truth.regions || []).forEach((region) => {
        const b = region.bbox;
        ctx.strokeRect(toX(b.x), toY(b.y), scaled(b.w), scaledY(b.h));
      });
      ctx.setLineDash([]);
    }
    if ($("ov-lines").checked) {
      (view.lines || []).forEach((line, index) => {
        const selected = state.selectedLine === index;
        const color = line.state === "committed" ? (line.accepted ? "rgba(127,242,168,.95)" : "rgba(255,196,112,.95)") : line.state === "failed" ? "rgba(255,111,125,.95)" : "rgba(100,232,225,.9)";
        ctx.strokeStyle = color;
        ctx.lineWidth = selected ? 2.4 : 1.2;
        ctx.strokeRect(toX(line.bbox.x), toY(line.bbox.y), scaled(line.bbox.w), scaledY(line.bbox.h));
        if (selected) {
          ctx.fillStyle = "rgba(100,232,225,.08)";
          ctx.fillRect(toX(line.bbox.x), toY(line.bbox.y), scaled(line.bbox.w), scaledY(line.bbox.h));
        }
        const label = String(line.reading_index).padStart(2, "0");
        ctx.font = "600 9px ui-monospace, monospace";
        const labelW = ctx.measureText(label).width + 8;
        ctx.fillStyle = color;
        ctx.fillRect(toX(line.bbox.x), Math.max(0, toY(line.bbox.y) - 14), labelW, 14);
        ctx.fillStyle = "#071014";
        ctx.fillText(label, toX(line.bbox.x) + 4, Math.max(10, toY(line.bbox.y) - 4));
      });
    }
    ctx.strokeStyle = "rgba(100,232,225,.24)";
    ctx.lineWidth = 1;
    ctx.strokeRect(offsetX, offsetY, drawW, drawH);
  }

  function canvasClick(event) {
    if (!state.image || !state.view) return;
    const canvas = $("viewport");
    const rect = canvas.getBoundingClientRect();
    const x = event.clientX - rect.left;
    const y = event.clientY - rect.top;
    const scale = Math.min(rect.width / state.image.width, rect.height / state.image.height);
    const offsetX = (rect.width - state.image.width * scale) / 2;
    const offsetY = (rect.height - state.image.height * scale) / 2;
    const ix = (x - offsetX) / scale;
    const iy = (y - offsetY) / scale;
    let best = -1;
    let bestArea = Infinity;
    (state.view.lines || []).forEach((line, index) => {
      const b = line.bbox;
      if (ix >= b.x && iy >= b.y && ix <= b.x + b.w && iy <= b.y + b.h && b.w * b.h < bestArea) {
        best = index;
        bestArea = b.w * b.h;
      }
    });
    if (best >= 0) {
      state.selectedLine = best;
      renderLines();
      renderInspector();
      drawCanvas();
    }
  }

  async function exportRun() {
    try {
      const data = state.jobId
        ? await call("run.export", { job_id: state.jobId })
        : await call("run.export_latest", { sample_id: state.sample.id });
      const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = "ocr-workbench-" + (state.sample ? state.sample.id : "run") + ".json";
      document.body.appendChild(link);
      link.click();
      link.remove();
      URL.revokeObjectURL(url);
      toast("Structured result exported", "ok");
    } catch (error) {
      showError(error);
    }
  }

  function escapeHTML(value) {
    return String(value ?? "")
      .replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;")
      .replaceAll(">", "&gt;")
      .replaceAll('"', "&quot;")
      .replaceAll("'", "&#039;");
  }

  function bind() {
    qsa(".tab").forEach((tab) => tab.addEventListener("click", () => switchTab(tab.dataset.tab)));
    $("btn-run").addEventListener("click", () => runPipeline("live"));
    $("btn-pause").addEventListener("click", pauseRun);
    $("btn-recompute").addEventListener("click", () => runPipeline("reset"));
    $("btn-resume").addEventListener("click", resumeRun);
    $("btn-cancel").addEventListener("click", cancelRun);
    $("btn-tune").addEventListener("click", tuneRun);
    $("btn-compare").addEventListener("click", compareRun);
    $("btn-export").addEventListener("click", exportRun);
    $("btn-generate").addEventListener("click", async () => {
      setBusy(true);
      try {
        const manifest = await call("samples.generate");
        state.samples = manifest.samples;
        state.bootstrap.manifest = manifest;
        renderSamples();
        toast("Deterministic sample set regenerated", "ok");
        if (state.samples.length) await selectSample(state.samples[0].id);
      } catch (error) {
        showError(error);
      } finally {
        setBusy(false);
      }
    });
    $("btn-reset-params").addEventListener("click", () => writeParams(state.bootstrap.defaults));
    $("modal-close").addEventListener("click", () => { $("modal").hidden = true; });
    $("modal").addEventListener("click", (event) => { if (event.target === $("modal")) $("modal").hidden = true; });
    $("viewport").addEventListener("click", canvasClick);
    window.addEventListener("resize", drawCanvas);
    if (window.ResizeObserver) new ResizeObserver(drawCanvas).observe($("canvas-wrap"));
    if (isWails() && window.runtime && window.runtime.EventsOn) {
      window.runtime.EventsOn("run:update", () => { if (state.jobId) pollOnce().catch(showError); });
      window.runtime.EventsOn("run:done", () => { if (state.jobId) pollOnce().catch(showError); });
    }
  }

  bind();
  bootstrap();
})();
