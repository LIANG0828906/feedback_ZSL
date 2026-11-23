(() => {
  const $ = (id) => document.getElementById(id);
  const state = {
    session: null,
    selected: new Set(),
    samples: [],
    polling: null,
    dirty: false,
  };

  const api = (name, ...args) => {
    if (!window.go?.main?.App?.[name]) {
      return Promise.reject(new Error("Wails 绑定不可用；请使用 wails build -tags wails -s 或 wails dev 启动桌面应用"));
    }
    return window.go.main.App[name](...args);
  };

  const parse = (raw) => {
    if (typeof raw === "string") return JSON.parse(raw);
    return raw;
  };

  const showToast = (message, isError = false) => {
    const toast = $("toast");
    toast.textContent = message;
    toast.classList.toggle("error", isError);
    toast.classList.add("show");
    clearTimeout(showToast.timer);
    showToast.timer = setTimeout(() => toast.classList.remove("show"), 4200);
  };

  const formatFloat = (v, digits = 4) => Number.isFinite(v) ? v.toFixed(digits) : "-";
  const shortHash = (v) => v ? `${v.slice(0, 12)}…${v.slice(-6)}` : "-";
  const escapeHTML = (value) => String(value ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));

  function paramsFromForm() {
    return {
      hashAlgorithm: $("hashAlgorithm").value,
      fingerprintGrid: Number($("fingerprintGrid").value),
      threshold: Number($("threshold").value),
      window: Number($("window").value),
      blockSize: Number($("blockSize").value),
      workerCount: Number($("workerCount").value),
      pixelBufferLimit: Number($("pixelBufferLimit").value),
      queueCapacity: Number($("queueCapacity").value),
      maskTolerance: state.session?.image?.maskTolerance ?? 0.08,
    };
  }

  function applyParams(session) {
    if (!session?.params || state.dirty) return;
    const p = session.params;
    if ($("hashAlgorithm").querySelector(`option[value="${p.hashAlgorithm}"]`)) $("hashAlgorithm").value = p.hashAlgorithm;
    if ($("fingerprintGrid").querySelector(`option[value="${p.fingerprintGrid}"]`)) $("fingerprintGrid").value = String(p.fingerprintGrid);
    if ($("blockSize").querySelector(`option[value="${p.blockSize}"]`)) $("blockSize").value = String(p.blockSize);
    if ($("window").querySelector(`option[value="${p.window}"]`)) $("window").value = String(p.window);
    $("threshold").value = String(p.threshold);
    $("thresholdValue").textContent = Number(p.threshold).toFixed(2);
    $("workerCount").value = String(p.workerCount);
    $("pixelBufferLimit").value = String(p.pixelBufferLimit);
    $("queueCapacity").value = String(p.queueCapacity);
  }

  function renderSession(session) {
    state.session = session;
    applyParams(session);
    const status = $("runStatus");
    status.className = "pill " + (session.running ? (session.paused ? "paused" : "running") : (session.lastError ? "error" : "idle"));
    status.textContent = session.running ? (session.paused ? "已暂停" : "分析中") : (session.lastError ? "需检查" : "空闲");

    const total = session.total || session.result?.summary?.totalBlocks || 0;
    const progress = session.progress || 0;
    $("progressBar").style.width = total ? `${Math.min(100, 100 * progress / total)}%` : "0%";
    $("runBtn").disabled = session.running;
    $("pauseBtn").disabled = !session.running || session.paused;
    $("resumeBtn").disabled = !session.running || !session.paused;
    $("cancelBtn").disabled = !session.running;
    $("retryBtn").disabled = session.running || state.selected.size === 0;
    $("selectedCount").textContent = String(state.selected.size);

    $("dbPath").textContent = session.dbPath || "-";
    $("dbPath").title = session.dbPath || "";
    if (session.image) {
      $("imageMeta").textContent = `${session.image.id} · ${session.image.width}×${session.image.height}×${session.image.channels}`;
      $("originalImage").src = `data:image/png;base64,${session.originalBase64}`;
    } else {
      $("imageMeta").textContent = "未加载";
    }
    if (session.result) {
      $("paramsVersion").textContent = shortHash(session.result.paramsVersion);
      $("paramsVersion").title = session.result.paramsVersion || "";
    }

    const result = session.result;
    if (!result) {
      $("emptyStage").style.display = "block";
      $("heatmapImage").style.display = "none";
      return;
    }
    $("emptyStage").style.display = "none";
    $("heatmapImage").style.display = "block";
    $("heatmapImage").src = `data:image/png;base64,${session.heatmapBase64}`;
    $("heatmapCaption").textContent = `heatmap ${result.heatmapSha256 || "-"} · key ${result.heatmapKey || "-"}`;

    const summary = result.summary || {};
    $("mTotal").textContent = summary.totalBlocks ?? "-";
    $("mBlockSize").textContent = `block ${result.blockSize}px · ${result.states?.length || 0} committed`;
    $("mTampered").textContent = summary.tamperedBlocks ?? "-";
    $("mMeanScore").textContent = `mean score ${formatFloat(summary.meanScore)}`;
    $("mReused").textContent = summary.reusedBlocks ?? "-";
    $("mRecomputed").textContent = `recomputed ${summary.recomputedBlocks ?? "-"}`;
    $("mAgreement").textContent = `${(100 * (result.truthAgreement || 0)).toFixed(1)}%`;
    $("mTruth").textContent = result.truthWithinTolerance ? "在声明容差内" : "超出声明容差";
    $("mHeatHash").textContent = shortHash(result.heatmapSha256);
    $("mHeatHash").title = result.heatmapSha256 || "";
    $("mHeatState").textContent = result.heatmapReused ? "复用持久化汇总" : "已重新汇总";

    renderMatrix(result);
    renderInvalidations(result);
    renderQueue(result);
    renderTable(result.states || []);
    renderComparison(session.comparison);
    drawGrid(result);
  }

  function statusSets(result) {
    const invalid = new Set();
    const reused = new Set();
    for (const inv of result.invalidations || []) if (inv.blockIndex >= 0) invalid.add(inv.blockIndex);
    for (const rec of result.reuses || []) if (rec.stage === "summary" || rec.stage === "analysis") reused.add(rec.blockIndex);
    return { invalid, reused };
  }

  function renderMatrix(result) {
    const chunks = result.chunks || [];
    const states = new Map((result.states || []).map((s) => [s.blockIndex, s]));
    const { invalid, reused } = statusSets(result);
    const cols = Math.max(1, ...chunks.map((c) => c.col + 1));
    const matrix = $("blockMatrix");
    matrix.style.gridTemplateColumns = `repeat(${cols}, minmax(18px, 1fr))`;
    matrix.innerHTML = "";
    for (const chunk of chunks) {
      const block = states.get(chunk.index);
      const cell = document.createElement("button");
      cell.className = "block-cell";
      if (reused.has(chunk.index)) cell.classList.add("reused");
      if (invalid.has(chunk.index)) cell.classList.add("invalid");
      if (block?.tampered) cell.classList.add("tampered");
      if (state.selected.has(chunk.index)) cell.classList.add("selected");
      cell.textContent = chunk.index;
      cell.title = `块 ${chunk.index} · ${chunk.x0},${chunk.y0} → ${chunk.x1},${chunk.y1} · score ${formatFloat(block?.stats?.tamperScore)}`;
      cell.addEventListener("click", () => {
        if (state.selected.has(chunk.index)) state.selected.delete(chunk.index);
        else state.selected.add(chunk.index);
        renderMatrix(result);
        $("selectedCount").textContent = String(state.selected.size);
        $("retryBtn").disabled = state.session?.running || state.selected.size === 0;
      });
      matrix.appendChild(cell);
    }
  }

  function renderInvalidations(result) {
    const items = result.invalidations || [];
    $("invalidationCount").textContent = String(items.length);
    const graph = result.dependencyGraph;
    $("depGraph").textContent = graph?.nodes
      ? `hash → analysis → summary → heatmap · ${graph.nodes.length} nodes / ${graph.edges?.length || 0} edges`
      : "hash → analysis → summary → heatmap";
    $("invalidationList").innerHTML = items.length ? items.map((inv) => `
      <div class="event">
        <span class="block">${inv.blockIndex < 0 ? "FINAL" : "#" + inv.blockIndex}</span>
        <span class="stage">${escapeHTML(inv.stage)}</span>
        <span class="reason" title="${escapeHTML(inv.reason)}">${escapeHTML(inv.reason)}</span>
      </div>`).join("") : `<p class="subtle">无失效节点（全部命中持久化缓存）。</p>`;
    const reuses = (result.reuses || []).slice(0, 120);
    $("reuseList").innerHTML = reuses.length ? reuses.map((rec) => `
      <div class="event">
        <span class="block">#${rec.blockIndex}</span>
        <span class="stage">${escapeHTML(rec.stage)}</span>
        <span class="reason">${escapeHTML(rec.reason)}</span>
      </div>`).join("") : `<p class="subtle">无复用记录。</p>`;
  }

  function renderQueue(result) {
    const events = result.queueEvents || [];
    $("queueEvents").innerHTML = events.length ? events.map((ev) => `
      <div class="event">
        <span class="block">#${ev.blockIndex}</span>
        <span class="stage">${escapeHTML(ev.stage)}</span>
        <span class="reason">${escapeHTML(ev.reason)}</span>
      </div>`).join("") : `<p class="subtle">未发生缓冲或队列阻塞。</p>`;
  }

  function renderTable(states) {
    $("blockTable").innerHTML = states.map((s) => `
      <tr>
        <td>${s.blockIndex}</td>
        <td>${s.x0},${s.y0} → ${s.x1},${s.y1}</td>
        <td class="hash" title="${escapeHTML(s.contentHash)}">${escapeHTML(s.contentHash)}</td>
        <td class="fp" title="${escapeHTML(s.fingerprint)}">${escapeHTML(s.fingerprint)}</td>
        <td>${formatFloat(s.stats.noiseVariance)}</td>
        <td>${formatFloat(s.stats.residualCoverage)}</td>
        <td>${formatFloat(s.stats.edgeConsistency)}</td>
        <td>${formatFloat(s.stats.contrast)}</td>
        <td>${formatFloat(s.stats.tamperScore)}</td>
        <td class="${s.tampered ? "bad-text" : "ok-text"}">${s.tampered ? "篡改" : "正常"}</td>
        <td>${s.commitSeq}</td>
      </tr>`).join("");
  }

  function renderComparison(comparison) {
    const badge = $("compareBadge");
    if (!comparison) {
      badge.className = "badge muted";
      badge.textContent = "未运行";
      return;
    }
    badge.className = "badge " + (comparison.equal ? "ok" : "bad");
    badge.textContent = comparison.equal ? "逐像素一致" : "存在分歧";
    const pixel = comparison.firstPixel;
    $("comparePane").innerHTML = `
      <p class="${comparison.equal ? "ok-text" : "bad-text"}">
        ${comparison.equal ? "增量、全量、串行三路径验证通过。" : escapeHTML(comparison.message || "路径结果不一致")}
      </p>
      ${pixel ? `<p class="warn-text">首个差异像素 (${pixel.x}, ${pixel.y}) · 块 #${pixel.blockIndex} · ${escapeHTML(pixel.stage)}</p>` : ""}
      <div class="compare-grid">
        <div class="compare-card"><span>增量</span><code title="${escapeHTML(comparison.incrementalHash)}">${escapeHTML(shortHash(comparison.incrementalHash))}</code></div>
        <div class="compare-card"><span>全量</span><code title="${escapeHTML(comparison.referenceHash)}">${escapeHTML(shortHash(comparison.referenceHash))}</code></div>
        <div class="compare-card"><span>串行</span><code title="${escapeHTML(comparison.serialHash)}">${escapeHTML(shortHash(comparison.serialHash))}</code></div>
      </div>
      <p class="subtle" style="margin-top:10px">heatmap ${comparison.heatmapEqual ? "相等" : "不等"} · per-block ${comparison.summaryEqual ? "相等" : "不等"} · commit sequence ${comparison.sequenceEqual ? "相等" : "不等"}</p>`;
  }

  function drawGrid(result) {
    const stage = $("imageStage");
    const canvas = $("gridCanvas");
    const rect = stage.getBoundingClientRect();
    if (!rect.width || !result.chunks?.length) return;
    const dpr = Math.min(2, window.devicePixelRatio || 1);
    canvas.width = Math.max(1, Math.floor(rect.width * dpr));
    canvas.height = Math.max(1, Math.floor(rect.height * dpr));
    const ctx = canvas.getContext("2d");
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    const imageW = state.session?.image?.width || 1;
    const imageH = state.session?.image?.height || 1;
    const scale = Math.min(rect.width / imageW, rect.height / imageH) * dpr;
    const offsetX = (canvas.width - imageW * scale) / 2;
    const offsetY = (canvas.height - imageH * scale) / 2;
    ctx.strokeStyle = "rgba(120, 170, 220, .24)";
    ctx.lineWidth = dpr;
    for (const chunk of result.chunks) {
      const x = offsetX + chunk.x0 * scale;
      const y = offsetY + chunk.y0 * scale;
      const w = (chunk.x1 - chunk.x0) * scale;
      const h = (chunk.y1 - chunk.y0) * scale;
      ctx.strokeRect(x, y, w, h);
      if (state.selected.has(chunk.index)) {
        ctx.strokeStyle = "rgba(85, 214, 190, .95)";
        ctx.lineWidth = 2 * dpr;
        ctx.strokeRect(x, y, w, h);
        ctx.strokeStyle = "rgba(120, 170, 220, .24)";
        ctx.lineWidth = dpr;
      }
    }
  }

  async function refresh() {
    try {
      const session = parse(await api("GetSession"));
      renderSession(session);
      if (session.running) startPolling();
      else stopPolling();
    } catch (err) {
      showToast(err.message || String(err), true);
    }
  }

  function startPolling() {
    if (state.polling) return;
    state.polling = setInterval(refresh, 320);
  }
  function stopPolling() {
    if (!state.polling) return;
    clearInterval(state.polling);
    state.polling = null;
  }

  async function loadSamples(manifestPath) {
    try {
      const data = parse(await api("ListSamples", manifestPath || ""));
      state.samples = data.images || [];
      const select = $("sampleSelect");
      select.innerHTML = state.samples.map((img) => `<option value="${escapeHTML(img.id)}">${escapeHTML(img.id)}</option>`).join("");
      if (state.session?.imageId) select.value = state.session.imageId;
    } catch (err) {
      showToast(err.message || String(err), true);
    }
  }

  async function openSelectedSample() {
    if (!state.samples.length || !state.session) return;
    const imageID = $("sampleSelect").value;
    try {
      const raw = await api("OpenWorkspace", state.session.manifestPath, imageID, state.session.dbPath);
      state.selected.clear();
      renderSession(parse(raw));
      showToast(`已加载 ${imageID}`);
    } catch (err) {
      showToast(err.message || String(err), true);
    }
  }

  async function runAnalysis(scopeBlocks = []) {
    try {
      const request = { params: paramsFromForm(), scopeBlocks };
      state.dirty = false;
      const raw = await api("RunJSON", JSON.stringify(request));
      const session = parse(raw);
      renderSession(session);
      if (session.running) startPolling();
      else {
        stopPolling();
        if (session.lastError) showToast(session.lastError, true);
        else showToast(scopeBlocks.length ? "逐块重试已完成" : "分析完成");
      }
    } catch (err) {
      showToast(err.message || String(err), true);
      refresh();
    }
  }

  function bind() {
    for (const id of ["threshold", "window", "blockSize", "hashAlgorithm", "fingerprintGrid", "workerCount", "queueCapacity", "pixelBufferLimit"]) {
      $(id).addEventListener("input", () => {
        state.dirty = true;
        if (id === "threshold") $("thresholdValue").textContent = Number($("threshold").value).toFixed(2);
      });
    }
    $("opacity").addEventListener("input", () => { $("heatmapImage").style.opacity = $("opacity").value; });
    $("runBtn").addEventListener("click", () => runAnalysis([]));
    $("retryBtn").addEventListener("click", () => runAnalysis([...state.selected].sort((a, b) => a - b)));
    $("pauseBtn").addEventListener("click", async () => {
      try { await api("Pause"); refresh(); } catch (err) { showToast(err.message || String(err), true); }
    });
    $("resumeBtn").addEventListener("click", async () => {
      try { await api("Resume"); refresh(); } catch (err) { showToast(err.message || String(err), true); }
    });
    $("cancelBtn").addEventListener("click", async () => {
      try { await api("Cancel"); showToast("已请求取消，已提交分块会保留为检查点"); refresh(); } catch (err) { showToast(err.message || String(err), true); }
    });
    $("verifyBtn").addEventListener("click", async () => {
      $("verifyBtn").disabled = true;
      $("verifyBtn").textContent = "验证中…";
      try {
        const raw = await api("VerifyThreeWay");
        renderSession(parse(raw));
        showToast("三路径验证完成");
      } catch (err) {
        showToast(err.message || String(err), true);
      } finally {
        $("verifyBtn").disabled = false;
        $("verifyBtn").textContent = "三路径比对";
      }
    });
    $("hashCheckBtn").addEventListener("click", async () => {
      try {
        const data = parse(await api("VerifyPersistedHashes"));
        showToast(data.message || "内容哈希一致");
      } catch (err) {
        showToast(err.message || String(err), true);
      }
    });
    $("exportBtn").addEventListener("click", async () => {
      try {
        const files = parse(await api("ExportReport", ""));
        showToast(`报告已导出：${files.reportJson}`);
      } catch (err) {
        showToast(err.message || String(err), true);
      }
    });
    $("resetBtn").addEventListener("click", async () => {
      if (!confirm("清空当前图像的 SQLite 检查点并全量重算？")) return;
      try {
        renderSession(parse(await api("ResetCheckpoint")));
        showToast("检查点已清空");
      } catch (err) {
        showToast(err.message || String(err), true);
      }
    });
    $("sampleSelect").addEventListener("change", openSelectedSample);
    window.addEventListener("resize", () => {
      if (state.session?.result) drawGrid(state.session.result);
    });
  }

  async function boot() {
    bind();
    try {
      const session = parse(await api("GetSession"));
      renderSession(session);
      await loadSamples(session.manifestPath);
    } catch (err) {
      showToast(err.message || String(err), true);
    }
  }

  document.addEventListener("DOMContentLoaded", boot);
})();
