const state = {
  data: null,
  layers: { shade: true, shadow: true, sky: false, seam: true },
  selected: { chunk: null, x: 2, z: 2 },
};

const canvas = document.getElementById("terrain");
const ctx = canvas.getContext("2d");
const timeSelect = document.getElementById("time");
const chunkSelect = document.getElementById("chunk");

function fmt(value, digits = 6) {
  if (value === null || value === undefined) return "—";
  return Number(value).toFixed(digits);
}

async function load(time = null) {
  const query = time === null ? "" : `?time=${encodeURIComponent(time)}`;
  const response = await fetch(`/api/state${query}`);
  if (!response.ok) throw new Error(await response.text());
  applyState(await response.json());
}

function applyState(data) {
  state.data = data;
  document.getElementById("seed").textContent = `seed ${data.seed}`;
  document.getElementById("status").textContent =
    `${data.chunks.length} 分块 · ${data.times.length} 个太阳时刻 · ${data.edited ? "编辑后" : "固定样本"}`;
  fillSelect(timeSelect, data.times, data.time);
  fillSelect(chunkSelect, data.chunks.map(c => c.id), state.selected.chunk || data.chunks[0]?.id);
  state.selected.chunk = chunkSelect.value;
  renderStats(data);
  renderLists(data);
  draw();
}

function fillSelect(select, values, current) {
  select.innerHTML = "";
  for (const value of values) {
    const option = document.createElement("option");
    option.value = String(value);
    option.textContent = String(value);
    if (String(value) === String(current)) option.selected = true;
    select.append(option);
  }
}

function renderStats(data) {
  const comparison = document.getElementById("comparison");
  comparison.textContent = data.comparison.equal ? "全量 / 任意顺序 / 增量逐项相等" : `${data.comparison.code}: ${data.comparison.error}`;
  comparison.className = "result" + (data.comparison.equal ? "" : " bad");
  document.getElementById("skyError").textContent = data.truth ? fmt(data.truth.max_sky_error, 9) : "编辑后不可用";
  document.getElementById("irrError").textContent = data.truth ? fmt(data.truth.max_irradiance_error, 9) : "编辑后不可用";
  document.getElementById("budget").textContent = `${data.stats.workers}/${data.stats.max_parallelism} · ${data.stats.queue_budget}`;
  document.getElementById("batches").textContent = String(data.stats.buffer_batches);
  document.getElementById("editHint").textContent = data.edited
    ? "已编辑；增量路径与当前全量路径比对，生成器真值仅对原始样本有效。"
    : "未编辑，真值对照有效。";
}

function renderLists(data) {
  const inv = document.getElementById("invalidations");
  inv.innerHTML = "";
  if (!data.invalidations?.length) {
    inv.innerHTML = "<li>暂无</li>";
  } else {
    for (const item of data.invalidations) {
      const li = document.createElement("li");
      li.textContent = `${item.chunk_id}@${item.time} · ${item.reason} · ${item.dependency}`;
      inv.append(li);
    }
  }
  const seams = document.getElementById("seams");
  seams.innerHTML = "";
  if (!data.seam?.edges?.length) {
    seams.innerHTML = "<li>暂无</li>";
  } else {
    for (const edge of data.seam.edges) {
      const transfers = data.seam.transfers?.filter(t => t.edge_id === edge).length || 0;
      const li = document.createElement("li");
      li.textContent = `${edge} · handoff=${transfers}`;
      seams.append(li);
    }
  }
}

function draw() {
  const data = state.data;
  if (!data) return;
  const width = canvas.width;
  const height = canvas.height;
  const pad = 30;
  const bounds = data.bounds;
  const scale = Math.min((width - pad * 2) / (bounds.max_x - bounds.min_x), (height - pad * 2) / (bounds.max_z - bounds.min_z));
  const ox = pad;
  const oy = pad;
  const tx = x => ox + (x - bounds.min_x) * scale;
  const ty = z => oy + (z - bounds.min_z) * scale;
  ctx.clearRect(0, 0, width, height);
  ctx.fillStyle = "#0d1117";
  ctx.fillRect(0, 0, width, height);

  for (const chunk of data.chunks) {
    const b = chunk.bounds;
    const avg = (chunk.average_irradiance[0] + chunk.average_irradiance[1] + chunk.average_irradiance[2]) / 3;
    if (state.layers.shade) {
      const v = Math.max(0, Math.min(1, avg / 5));
      ctx.fillStyle = `hsl(${110 + v * 35} 30% ${14 + v * 34}%)`;
    } else {
      ctx.fillStyle = "#151b23";
    }
    ctx.fillRect(tx(b.min_x), ty(b.min_z), (b.max_x - b.min_x) * scale, (b.max_z - b.min_z) * scale);
    ctx.strokeStyle = "#3b4a5b";
    ctx.lineWidth = 1;
    ctx.strokeRect(tx(b.min_x), ty(b.min_z), (b.max_x - b.min_x) * scale, (b.max_z - b.min_z) * scale);
    ctx.fillStyle = "#afc0d3";
    ctx.font = "12px ui-monospace, monospace";
    ctx.fillText(chunk.id, tx(b.min_x) + 7, ty(b.min_z) + 17);

    for (const point of chunk.points) {
      const px = tx(point.world.x);
      const py = ty(point.world.z);
      if (state.layers.shadow) {
        ctx.beginPath();
        ctx.arc(px, py, point.direct_shadow ? 4.3 : 2.6, 0, Math.PI * 2);
        ctx.fillStyle = point.direct_shadow ? "#ff5f64" : "#7fd79b";
        ctx.globalAlpha = point.direct_shadow ? .95 : .58;
        ctx.fill();
      }
      if (state.layers.sky) {
        ctx.beginPath();
        ctx.arc(px, py, 2 + point.sky_visibility * 8, 0, Math.PI * 2);
        ctx.strokeStyle = "#6aa9ff";
        ctx.globalAlpha = .22 + point.sky_visibility * .7;
        ctx.lineWidth = 1.4;
        ctx.stroke();
      }
      ctx.globalAlpha = 1;
    }
  }

  if (state.layers.seam && data.edges) {
    ctx.save();
    ctx.setLineDash([7, 5]);
    ctx.lineWidth = 3;
    ctx.strokeStyle = "#f4c95d";
    for (const edge of data.edges) {
      ctx.beginPath();
      let x1, z1, x2, z2;
      if (edge.axis === "x") {
        x1 = x2 = edge.position; z1 = edge.start; z2 = edge.end;
      } else {
        z1 = z2 = edge.position; x1 = edge.start; x2 = edge.end;
      }
      ctx.moveTo(tx(x1), ty(z1));
      ctx.lineTo(tx(x2), ty(z2));
      ctx.stroke();
      ctx.fillStyle = "#f4c95d";
      ctx.font = "10px ui-monospace, monospace";
      ctx.fillText(edge.id, tx((x1 + x2) / 2) + 4, ty((z1 + z2) / 2) - 4);
    }
    ctx.restore();
  }

  const invalidChunks = new Set((data.invalidations || []).filter(i => i.time === data.time).map(i => i.chunk_id));
  for (const chunk of data.chunks) {
    if (!invalidChunks.has(chunk.id)) continue;
    const b = chunk.bounds;
    ctx.strokeStyle = "#bd7cff";
    ctx.lineWidth = 4;
    ctx.strokeRect(tx(b.min_x) + 2, ty(b.min_z) + 2, (b.max_x - b.min_x) * scale - 4, (b.max_z - b.min_z) * scale - 4);
  }
}

function pick(event) {
  if (!state.data) return;
  const rect = canvas.getBoundingClientRect();
  const px = (event.clientX - rect.left) * canvas.width / rect.width;
  const py = (event.clientY - rect.top) * canvas.height / rect.height;
  const pad = 30;
  const b = state.data.bounds;
  const scale = Math.min((canvas.width - pad * 2) / (b.max_x - b.min_x), (canvas.height - pad * 2) / (b.max_z - b.min_z));
  const x = b.min_x + (px - pad) / scale;
  const z = b.min_z + (py - pad) / scale;
  for (const chunk of state.data.chunks) {
    if (x >= chunk.bounds.min_x && x <= chunk.bounds.max_x && z >= chunk.bounds.min_z && z <= chunk.bounds.max_z) {
      chunkSelect.value = chunk.id;
      state.selected.chunk = chunk.id;
      const gx = Math.round((x - chunk.bounds.min_x) / (chunk.bounds.max_x - chunk.bounds.min_x) * (chunk.res_x - 1));
      const gz = Math.round((z - chunk.bounds.min_z) / (chunk.bounds.max_z - chunk.bounds.min_z) * (chunk.res_z - 1));
      document.getElementById("vx").value = String(gx);
      document.getElementById("vz").value = String(gz);
      draw();
      return;
    }
  }
}

document.querySelectorAll("[data-layer]").forEach(button => {
  button.addEventListener("click", () => {
    const key = button.dataset.layer;
    state.layers[key] = !state.layers[key];
    button.classList.toggle("active", state.layers[key]);
    draw();
  });
});
timeSelect.addEventListener("change", () => load(Number(timeSelect.value)));
chunkSelect.addEventListener("change", () => { state.selected.chunk = chunkSelect.value; draw(); });
canvas.addEventListener("click", pick);
document.getElementById("raise").addEventListener("click", async () => {
  const body = {
    chunk_id: chunkSelect.value,
    x: Number(document.getElementById("vx").value),
    z: Number(document.getElementById("vz").value),
    delta: Number(document.getElementById("delta").value),
  };
  const response = await fetch("/api/raise", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if (!response.ok) {
    alert(await response.text());
    return;
  }
  applyState(await response.json());
});
document.getElementById("reset").addEventListener("click", async () => {
  const response = await fetch("/api/reset", { method: "POST" });
  if (!response.ok) {
    alert(await response.text());
    return;
  }
  applyState(await response.json());
});

load().catch(error => {
  document.getElementById("status").textContent = String(error);
});
