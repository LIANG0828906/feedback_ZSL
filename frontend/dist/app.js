const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

const app = {
  state: null,
  samples: [],
  selectedEntry: null,
  activeView: "final",
  bottomTab: "matrix",
  dragId: null,
  draft: null,
};

function hasWails() {
  return !!(window.go && window.go.main && window.go.main.App);
}

async function invoke(method, ...args) {
  if (hasWails() && typeof window.go.main.App[method] === "function") {
    return window.go.main.App[method](...args);
  }
  const map = {
    ListSamples: ["GET", "/api/samples"],
    LoadSample: ["POST", "/api/load"],
    ApplyEdit: ["POST", "/api/edit"],
    Reorder: ["POST", "/api/reorder"],
    RetrySegment: ["POST", "/api/retry"],
    ComparePaths: ["POST", "/api/compare"],
    Export: ["POST", "/api/export"],
  };
  const entry = map[method];
  if (!entry) throw new Error(`No bridge for ${method}`);
  const [verb, url] = entry;
  const resp = await fetch(url, {
    method: verb,
    headers: { "Content-Type": "application/json" },
    body: verb === "GET" ? undefined : JSON.stringify(args[0] ?? {}),
  });
  const data = await resp.json();
  if (!resp.ok) throw data.error ?? data;
  return data;
}

function parseError(err) {
  if (!err) return "Unknown error";
  if (typeof err === "string") {
    try { err = JSON.parse(err); } catch (_) { return err; }
  }
  if (err.code) {
    const bits = [err.code, err.message];
    if (err.entry_index) bits.push(`entry ${err.entry_index}`);
    if (err.field) bits.push(`field ${err.field}`);
    if (err.segment) bits.push(`segment ${err.segment}`);
    return bits.filter(Boolean).join(" · ");
  }
  return err.message || JSON.stringify(err);
}

function setLoading(show, title = "", copy = "") {
  $("#loading").classList.toggle("show", show);
  if (title) $("#loadingTitle").textContent = title;
  if (copy) $("#loadingCopy").textContent = copy;
}

let toastTimer;
function toast(message, isError = false) {
  const el = $("#toast");
  el.textContent = message;
  el.classList.toggle("err", isError);
  el.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove("show"), isError ? 9000 : 4200);
}

async function boot() {
  setLoading(true);
  try {
    app.samples = await invoke("ListSamples");
    const sel = $("#sampleSelect");
    sel.innerHTML = app.samples.map(s =>
      `<option value="${s.name}">${s.name} · ${s.width}×${s.height} · ${s.depth}-bit ${s.layout}</option>`
    ).join("");
    if (app.samples.length) await loadSample(app.samples[0].name);
  } catch (err) {
    toast(parseError(err), true);
  } finally {
    setLoading(false);
  }
}

async function loadSample(name) {
  setLoading(true, "Running deterministic pipeline…", "Loading source, validating plan, resuming checkpoints and verifying truth.");
  try {
    app.state = await invoke("LoadSample", name);
    app.selectedEntry = app.state.entries[0]?.id ?? null;
    app.activeView = "final";
    renderAll();
    $("#retrySegment").value = 0;
  } catch (err) {
    toast(parseError(err), true);
  } finally {
    setLoading(false);
  }
}

function renderAll() {
  if (!app.state) return;
  const s = app.state;
  const sample = app.samples.find(x => x.name === s.sample.name);
  if (sample) $("#sampleSelect").value = sample.name;
  $("#algoPill").textContent = s.algorithm_version;
  $("#revisionBadge").textContent = `rev ${s.plan.revision}`;
  const tp = $("#truthPill");
  tp.textContent = s.is_truth_revision ? "truth: current plan" : "truth: stale after edit";
  tp.classList.toggle("ok", s.is_truth_revision);
  tp.classList.toggle("bad", !s.is_truth_revision);
  renderPipeline();
  renderInspector();
  renderImages();
  renderMetrics();
  renderMatrix();
  renderAudit();
  renderLogs();
  renderVerification();
}

function renderPipeline() {
  const el = $("#pipeline");
  el.innerHTML = app.state.entries.map(e => `
    <div class="pipeline-item ${e.id === app.selectedEntry ? "selected" : ""}" draggable="true" data-id="${e.id}">
      <div class="entry-top">
        <div class="entry-index">${String(e.index).padStart(2, "0")}</div>
        <div class="entry-name">${escapeHTML(e.name || e.id)}</div>
        <div class="drag-handle" title="Drag to reorder">⋮⋮</div>
      </div>
      <div class="entry-spec">
        <span class="spec hot">${e.target_depth}-bit</span>
        <span class="spec">${e.target_channels} ch</span>
        <span class="spec">${e.target_layout}</span>
        <span class="spec">${e.alpha}</span>
        <span class="spec">${e.quantize}</span>
      </div>
      <div class="entry-regions">${e.regions.join(" · ")}</div>
    </div>
  `).join("");
  $$(".pipeline-item").forEach(item => {
    item.addEventListener("click", () => {
      app.selectedEntry = item.dataset.id;
      if (app.activeView === "entry") app.activeView = "entry";
      renderAll();
    });
    item.addEventListener("dragstart", (ev) => {
      app.dragId = item.dataset.id;
      item.classList.add("dragging");
      ev.dataTransfer.effectAllowed = "move";
      ev.dataTransfer.setData("text/plain", item.dataset.id);
    });
    item.addEventListener("dragend", () => {
      item.classList.remove("dragging");
      app.dragId = null;
    });
    item.addEventListener("dragover", (ev) => ev.preventDefault());
    item.addEventListener("drop", async (ev) => {
      ev.preventDefault();
      const source = app.dragId;
      const target = item.dataset.id;
      if (!source || source === target) return;
      const order = app.state.entries.map(e => e.id);
      const from = order.indexOf(source);
      const to = order.indexOf(target);
      order.splice(from, 1);
      order.splice(to, 0, source);
      await reorder(order);
    });
  });
  const pct = app.state.total_segments ? Math.round(100 * app.state.committed_segments / app.state.total_segments) : 0;
  $("#progressBar").style.width = `${pct}%`;
  $("#progressText").textContent = `${app.state.committed_segments} / ${app.state.total_segments} segments`;
}

async function reorder(order) {
  setLoading(true, "Reordering stage graph…", "Freezing committed snapshots and recomputing only the affected downstream coverage.");
  try {
    app.state = await invoke("Reorder", { order });
    renderAll();
    toast("Stage order updated; incremental path matched full recomputation.");
  } catch (err) {
    toast(parseError(err), true);
  } finally {
    setLoading(false);
  }
}

function selectedEntry() {
  return app.state?.entries.find(e => e.id === app.selectedEntry) ?? null;
}

function renderInspector() {
  const e = selectedEntry();
  const host = $("#inspector");
  if (!e) {
    host.className = "inspector-empty";
    host.textContent = "Choose a pipeline entry to edit its deterministic parameters.";
    $("#inspectorTitle").textContent = "Select an entry";
    return;
  }
  host.className = "inspector-form";
  $("#inspectorTitle").textContent = e.name || e.id;
  const srcCount = e.source_channels;
  const perm = e.permutation.slice();
  const target = e.target_channels;
  const channelNames = ["R", "G", "B", "A"];
  let channelRows = "";
  for (let slot = 0; slot < target; slot++) {
    const current = perm[slot] ?? -1;
    const opts = [];
    for (let s = 0; s < srcCount; s++) {
      opts.push(`<option value="${s}" ${current === s ? "selected" : ""}>${channelNames[s]} · source ${s}</option>`);
    }
    if (target > srcCount) opts.push(`<option value="-1" ${current === -1 ? "selected" : ""}>Fill channel</option>`);
    channelRows += `
      <div class="channel-row">
        <span class="slot-label">slot ${slot}</span>
        <select data-slot="${slot}">${opts.join("")}</select>
      </div>`;
  }
  const fillMap = new Map((e.fill || []).map(f => [f.target_index, f]));
  for (let slot = 0; slot < target; slot++) {
    if ((perm[slot] ?? -1) === -1 && !fillMap.has(slot)) {
      fillMap.set(slot, {
        target_index: slot, source: "constant", channel: "A",
        value: e.target_depth === 16 ? 65535 : 255,
      });
    }
  }
  const fills = Array.from(fillMap.values()).sort((a, b) => a.target_index - b.target_index);
  const fillRows = fills.map((f, idx) => {
    const sourceOpts = ["constant", "zero", "one", "alpha", "channel"].map(v =>
      `<option value="${v}" ${f.source === v ? "selected" : ""}>${v}</option>`).join("");
    return `
      <div class="fill-box" data-fill="${idx}">
        <div class="section-title">Fill slot ${f.target_index}</div>
        <select data-fill-source>${sourceOpts}</select>
        <select data-fill-channel>
          ${["A","R","G","B"].map(c => `<option value="${c}" ${f.channel === c ? "selected" : ""}>channel ${c}</option>`).join("")}
        </select>
        <select data-fill-source-channel>
          ${Array.from({length: srcCount}, (_, i) => `<option value="${i}" ${f.source_channel === i ? "selected" : ""}>source channel ${i} (${channelNames[i]})</option>`).join("")}
        </select>
        <input data-fill-value type="number" min="0" value="${f.value ?? 0}" />
      </div>`;
  }).join("");
  host.innerHTML = `
    <div class="inspector-section">
      <div class="section-title">Target format</div>
      <div class="form-row">
        <label>Bit depth</label>
        <div class="segmented" data-control="depth">
          <button data-value="8" class="${e.target_depth === 8 ? "active" : ""}">8-bit</button>
          <button data-value="16" class="${e.target_depth === 16 ? "active" : ""}">16-bit</button>
        </div>
      </div>
      <div class="form-row">
        <label>Channels</label>
        <select id="targetChannels">
          <option value="3" ${target === 3 ? "selected" : ""}>3 channels</option>
          <option value="4" ${target === 4 ? "selected" : ""}>4 channels</option>
        </select>
      </div>
      <div class="form-row">
        <label>Alpha convention</label>
        <select id="alphaMode">
          <option value="straight" ${e.alpha === "straight" ? "selected" : ""}>straight</option>
          <option value="premultiplied" ${e.alpha === "premultiplied" ? "selected" : ""}>premultiplied</option>
        </select>
      </div>
      <div class="form-row">
        <label>Quantize</label>
        <select id="quantizeMode">
          ${["floor","round","ceil"].map(v => `<option value="${v}" ${e.quantize === v ? "selected" : ""}>${v}</option>`).join("")}
        </select>
      </div>
    </div>
    <div class="inspector-section">
      <div class="section-title">Channel permutation · target slots</div>
      <div class="channel-map" id="channelMap">${channelRows}</div>
      ${fillRows}
      <div class="drop-line" id="dropLine"></div>
      <div class="hint">Source layout ${e.source_channels} ch · target ${target} ch · ${e.target_layout}. Decreasing channels records the discarded source indices; increasing channels requires an explicit fill.</div>
    </div>
    <div class="apply-row">
      <button class="btn" id="applyEditBtn">Apply &amp; recompute</button>
      <button class="btn ghost small" id="resetDraftBtn">Reset</button>
    </div>
    <div class="hint">Committed segment snapshots remain frozen. The service increments the plan revision before validating your edit.</div>
  `;
  host.querySelectorAll("[data-control=depth] button").forEach(btn => {
    btn.addEventListener("click", () => {
      host.querySelectorAll("[data-control=depth] button").forEach(b => b.classList.remove("active"));
      btn.classList.add("active");
    });
  });
  $("#targetChannels").addEventListener("change", () => {
    const newTarget = Number($("#targetChannels").value);
    const next = app.state.entries.find(x => x.id === e.id);
    next.target_channels = newTarget;
    next.permutation = defaultPermutation(srcCount, newTarget, next.permutation, next.fill);
    renderInspector();
  });
  updateDropLine();
  $$("#channelMap select").forEach(sel => sel.addEventListener("change", updateDropLine));
  $("#applyEditBtn").addEventListener("click", applyEdit);
  $("#resetDraftBtn").addEventListener("click", renderInspector);
}

function defaultPermutation(src, target, old, fills) {
  if (target < src) return Array.from({length: target}, (_, i) => i);
  if (target > src) {
    const out = Array.from({length: src}, (_, i) => i);
    while (out.length < target) out.push(-1);
    return out;
  }
  return old.length === target ? old.slice() : Array.from({length: target}, (_, i) => i);
}

function updateDropLine() {
  const e = selectedEntry();
  if (!e) return;
  const target = Number($("#targetChannels")?.value ?? e.target_channels);
  const used = $$("#channelMap select").map(s => Number(s.value)).filter(v => v >= 0);
  const drops = [];
  for (let i = 0; i < e.source_channels; i++) if (!used.includes(i)) drops.push(i);
  $("#dropLine").innerHTML = `Discarded source channels: <code>${drops.length ? JSON.stringify(drops) : "[]"}</code>`;
}

async function applyEdit() {
  const e = selectedEntry();
  if (!e) return;
  const targetDepth = Number($("[data-control=depth] button.active")?.dataset.value ?? e.target_depth);
  const targetChannels = Number($("#targetChannels").value);
  const permutation = $$("#channelMap select").map(s => Number(s.value));
  const fills = [];
  $$(".fill-box").forEach(box => {
    const targetIndex = Number(box.querySelector(".section-title").textContent.match(/\d+/)?.[0] ?? 0);
    const source = box.querySelector("[data-fill-source]").value;
    const channel = box.querySelector("[data-fill-channel]").value;
    const sourceChannel = Number(box.querySelector("[data-fill-source-channel]").value);
    const value = Number(box.querySelector("[data-fill-value]").value);
    fills.push({ target_index: targetIndex, source, source_channel: sourceChannel, channel, value });
  });
  const req = {
    entry_id: e.id,
    target_depth: targetDepth,
    target_channels: targetChannels,
    permutation,
    fill: fills,
    alpha: $("#alphaMode").value,
    quantize: $("#quantizeMode").value,
  };
  setLoading(true, "Recomputing affected segments…", "Incremental cache write and full-path comparison are running in parallel-safe order.");
  try {
    app.state = await invoke("ApplyEdit", req);
    renderAll();
    toast("Edit committed. Incremental output is pixel-identical to full recomputation.");
  } catch (err) {
    toast(parseError(err), true);
  } finally {
    setLoading(false);
  }
}

function renderImages() {
  const s = app.state;
  if (!s) return;
  const entry = selectedEntry();
  let view = s.final;
  let title = "Converted result";
  if (app.activeView === "source") {
    view = s.source;
    title = "Deterministic source";
  } else if (app.activeView === "diff") {
    view = s.diff;
    title = "Pixel difference heatmap";
  } else if (app.activeView === "entry" && entry) {
    view = s.entry_images[entry.id] || s.final;
    title = `Entry ${String(entry.index).padStart(2, "0")} · ${entry.name}`;
  }
  $("#mainImage").src = view.data_url;
  $("#viewerTitle").textContent = title;
  const meta = `${view.width ?? s.sample.width}×${view.height ?? s.sample.height}` +
    (view.depth ? ` · ${view.depth}-bit · ${view.channels}ch · ${view.layout} · ${view.alpha}` : "");
  $("#imageMeta").textContent = meta;
  $("#mResolution").textContent = `${s.sample.width}×${s.sample.height}`;
  $("#mFormat").textContent = `${s.sample.depth}-bit ${s.sample.layout}`;
  $("#mAlpha").textContent = view.alpha || s.entries.at(-1)?.alpha || "--";
  $("#entryTab").hidden = !entry;
  $$("#imageTabs button").forEach(b => b.classList.toggle("active", b.dataset.view === app.activeView));
}

function renderMetrics() {
  const s = app.state;
  $("#mChanged").textContent = s.diff_summary.changed_pixels.toLocaleString();
  $("#diffMax").textContent = `${s.diff_summary.max_channel_delta} / ${s.diff_summary.max_channel_value || 255}`;
  $("#diffCopy").textContent = s.diff_summary.changed_pixels
    ? `${s.diff_summary.changed_pixels.toLocaleString()} pixels changed versus ${s.diff_summary.compared_to}. Red indicates the largest channel delta.`
    : "No pixel delta in this comparison.";
}

function renderMatrix() {
  const s = app.state;
  const cells = s.matrix.cells;
  const cols = s.matrix.total_segments;
  const entries = s.entries;
  const byKey = new Map(cells.map(c => [`${c.entry_index}/${c.segment}`, c]));
  let html = `<div class="matrix-head matrix-row-label">Entry / segment</div>`;
  for (let seg = 0; seg < cols; seg++) html += `<div class="matrix-head">S${String(seg).padStart(2,"0")}</div>`;
  for (const e of entries) {
    html += `<div class="matrix-cell matrix-row-label" title="${escapeHTML(e.name)}">${String(e.index).padStart(2,"0")} ${escapeHTML(e.id)}</div>`;
    for (let seg = 0; seg < cols; seg++) {
      const c = byKey.get(`${e.index}/${seg}`) || {state:"pending"};
      let cls = c.state || "pending";
      if (c.invalidated) cls = "invalid";
      else if (c.reused && c.state === "committed") cls = "reused";
      const label = c.reused ? "↺" : c.invalidated ? "Δ" : c.state === "committed" ? "✓" : c.state === "failed" ? "!" : "·";
      const title = `entry ${e.index} ${e.id} · segment ${seg} · ${c.state}${c.reason ? " · " + c.reason : ""}${c.cache_key ? " · key " + c.cache_key.slice(0,12) : ""}`;
      html += `<div class="matrix-cell ${cls}" title="${escapeHTML(title)}">${label}</div>`;
    }
  }
  $("#matrix").style.gridTemplateColumns = `160px repeat(${cols}, minmax(34px, 1fr))`;
  $("#matrix").innerHTML = html;
}

function renderAudit() {
  const rows = app.state.audit;
  $("#auditBody").innerHTML = rows.map(c => {
    const result = c.invalidated ? "recomputed" : c.reused ? (c.reused ? "reused" : "copy") : "—";
    const cls = c.invalidated ? "invalid" : c.reused ? "reused" : c.state;
    return `<tr>
      <td>${String(c.entry_index).padStart(2,"0")} <span class="mono">${escapeHTML(c.entry_id)}</span></td>
      <td class="mono">S${String(c.segment).padStart(2,"0")}</td>
      <td>${(c.regions || []).map(escapeHTML).join(" · ") || "—"}</td>
      <td><span class="state-tag ${cls}">${c.state}</span></td>
      <td>${result}</td>
      <td>${escapeHTML(c.reason || "—")}</td>
      <td class="mono">${c.cache_key ? c.cache_key.slice(0, 12) + "…" : "—"}</td>
      <td class="mono">${c.snapshot_hash ? c.snapshot_hash.slice(0, 8) : "—"}</td>
    </tr>`;
  }).join("");
}

function renderLogs() {
  $("#logBody").innerHTML = app.state.logs.length
    ? app.state.logs.map(l => `<tr><td class="mono">${l.segment >= 0 ? "S" + String(l.segment).padStart(2,"0") : "run"}</td><td>${l.worker || "—"}</td><td>${escapeHTML(l.reason)}</td><td>${escapeHTML(l.detail)}</td></tr>`).join("")
    : `<tr><td colspan="4" style="color:var(--dim)">No resource waits recorded.</td></tr>`;
}

function renderVerification() {
  const v = app.state.verification;
  const card = $("#verificationCard");
  card.className = "verification-card " + (v.status === "MATCH" ? "match" : v.status === "DIFF" ? "diff" : "stale");
  $("#verifyStatus").textContent = v.status || "UNKNOWN";
  $("#verifyMessage").textContent = v.message || (v.status === "MATCH" ? "Independent verifier matched persisted segments, record and truth fingerprint." : "Run comparison to refresh verification.");
  const detail = $("#verificationDetail");
  detail.innerHTML = `
    <h3>Independent verification</h3>
    <p>${v.status === "MATCH" ? "Persisted segment pixels, plan, truth fingerprint and the main run record agree." : escapeHTML(v.message || "No successful verification report for this revision.")}</p>
    <div class="kv">
      <span>Status</span><strong>${escapeHTML(v.status || "UNKNOWN")}</strong>
      <span>Code</span><strong>${escapeHTML(v.code || "—")}</strong>
      <span>Truth fingerprint</span><code>${escapeHTML(v.truth_fingerprint || "—")}</code>
      <span>Report</span><code>${escapeHTML(v.report_path || "—")}</code>
      <span>Plan file</span><code>${escapeHTML(app.state.plan_path || "—")}</code>
      <span>Truth file</span><code>${escapeHTML(app.state.truth_path || "—")}</code>
      <span>Run record</span><code>${escapeHTML(app.state.record_path)}</code>
      <span>Checkpoint directory</span><code>${escapeHTML(app.state.checkpoint_dir)}</code>
      <span>Algorithm</span><code>${escapeHTML(app.state.algorithm_version)}</code>
      <span>Final digest</span><code>${escapeHTML(app.state.final_digest || "—")}</code>
      <span>Commit sequence</span><code>${escapeHTML(app.state.commit_sequence.join(", "))}</code>
      <span>Cache</span><code>${app.state.cache_hits} hits · ${app.state.reused_cells} reused · ${app.state.invalidated_cells} invalidated</code>
    </div>
    <div class="code-block">${escapeHTML(JSON.stringify({
      status: v.status, code: v.code || null, message: v.message || null,
      plan_fingerprint: app.state.plan_fingerprint,
      truth_fingerprint: v.truth_fingerprint || null,
    }, null, 2))}</div>
  `;
}

async function comparePaths() {
  setLoading(true, "Comparing incremental and full paths…", "Rebuilding from an empty cache and comparing stage hashes, pixels and commit order.");
  try {
    const out = await invoke("ComparePaths");
    app.state.verification = out.verification;
    renderVerification();
    if (out.equal) {
      toast(`Paths match · digest ${out.full_digest.slice(0, 16)}… · ${out.commit_sequence.length} commits`);
    } else {
      toast("Path comparison returned E_DIFF.", true);
    }
  } catch (err) {
    toast(parseError(err), true);
  } finally {
    setLoading(false);
  }
}

async function exportBundle() {
  setLoading(true, "Exporting deterministic bundle…", "Writing final PNG, plan, run record, verification report and export manifest.");
  try {
    const out = await invoke("Export");
    toast(`Exported to ${out.directory}`);
  } catch (err) {
    toast(parseError(err), true);
  } finally {
    setLoading(false);
  }
}

async function retrySegment() {
  const seg = Number($("#retrySegment").value);
  setLoading(true, `Retrying segment S${String(seg).padStart(2,"0")}…`, "Creating a new parameter revision, then recomputing only the forced segment.");
  try {
    app.state = await invoke("RetrySegment", { segment: seg });
    renderAll();
    toast(`Segment S${String(seg).padStart(2,"0")} retried; full-path comparison matched.`);
  } catch (err) {
    toast(parseError(err), true);
  } finally {
    setLoading(false);
  }
}

function escapeHTML(v) {
  return String(v ?? "").replace(/[&<>"']/g, c => ({ "&":"&amp;", "<":"&lt;", ">":"&gt;", '"':"&quot;", "'":"&#39;" }[c]));
}

$("#sampleSelect").addEventListener("change", (e) => loadSample(e.target.value));
$$("#imageTabs button").forEach(btn => btn.addEventListener("click", () => {
  app.activeView = btn.dataset.view;
  renderImages();
}));
$$("#bottomTabs button").forEach(btn => btn.addEventListener("click", () => {
  app.bottomTab = btn.dataset.tab;
  $$("#bottomTabs button").forEach(b => b.classList.toggle("active", b === btn));
  $$(".tab-pane").forEach(p => p.classList.toggle("active", p.id === `pane-${app.bottomTab}`));
}));
$("#compareBtn").addEventListener("click", comparePaths);
$("#refreshVerifyBtn").addEventListener("click", comparePaths);
$("#exportBtn").addEventListener("click", exportBundle);
$("#retryBtn").addEventListener("click", retrySegment);

boot();
