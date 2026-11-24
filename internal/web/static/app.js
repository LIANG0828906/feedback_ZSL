// Local-only diagnostic dashboard. All data comes from /api/data on the same
// origin; there are no external requests.
(function () {
  "use strict";

  const canvas = document.getElementById("canvas");
  const ctx = canvas.getContext("2d");
  const sideTitle = document.getElementById("side-title");
  const sideBody = document.getElementById("side-body");
  const meta = document.getElementById("meta");
  const zoneColors = ["#4cc2ff", "#7ee787", "#ffb454", "#d2a8ff", "#ff9bce"];
  const rarityColors = ["#c9d1d9", "#4cc2ff", "#ffb454"];
  let data = null;
  let view = "overview";

  fetch("/api/data")
    .then((r) => r.json())
    .then((payload) => {
      data = payload;
      meta.textContent =
        "region=" + data.region.name +
        " seed=0x" + data.region.seed.toString(16) +
        " zones=" + data.region.zones.length +
        " algo=" + data.report.algoVersion;
      draw();
    })
    .catch((err) => {
      meta.textContent = "数据加载失败: " + err;
    });

  document.querySelectorAll(".tabs button").forEach((btn) => {
    btn.addEventListener("click", () => {
      document.querySelectorAll(".tabs button").forEach((b) => b.classList.remove("active"));
      btn.classList.add("active");
      view = btn.dataset.view;
      draw();
    });
  });

  function scale() {
    const bbox = data.region.bbox;
    const pad = 40;
    const sx = (canvas.width - pad * 2) / (bbox.maxX - bbox.minX);
    const sy = (canvas.height - pad * 2) / (bbox.maxY - bbox.minY);
    const s = Math.min(sx, sy);
    return {
      s,
      x: (p) => pad + (p.x - bbox.minX) * s,
      y: (p) => canvas.height - pad - (p.y - bbox.minY) * s,
    };
  }

  function rect(t, r) {
    const x = t.x({ x: r.minX, y: 0 });
    const yTop = t.y({ x: 0, y: r.maxY });
    const w = (r.maxX - r.minX) * t.s;
    const h = (r.maxY - r.minY) * t.s;
    return { x, y: yTop, w, h };
  }

  function drawZones(t) {
    data.region.zones.forEach((zone, i) => {
      const rr = rect(t, zone.bounds);
      ctx.fillStyle = zoneColors[i % zoneColors.length] + "33";
      ctx.fillRect(rr.x, rr.y, rr.w, rr.h);
      ctx.strokeStyle = zoneColors[i % zoneColors.length];
      ctx.lineWidth = 2;
      ctx.strokeRect(rr.x, rr.y, rr.w, rr.h);
      ctx.fillStyle = "#e6edf3";
      ctx.font = "13px sans-serif";
      ctx.fillText("Z" + zone.id, rr.x + 8, rr.y + 18);
    });
  }

  function drawNoCollect(t) {
    ctx.save();
    ctx.setLineDash([6, 4]);
    data.region.noCollect.forEach((nz) => {
      const rr = rect(t, nz.bounds);
      ctx.fillStyle = "rgba(255,107,107,.25)";
      ctx.fillRect(rr.x, rr.y, rr.w, rr.h);
      ctx.strokeStyle = "#ff6b6b";
      ctx.lineWidth = 2;
      ctx.strokeRect(rr.x, rr.y, rr.w, rr.h);
      ctx.fillStyle = "#ff6b6b";
      ctx.fillText("禁采#" + nz.id, rr.x + 6, rr.y + 16);
    });
    ctx.restore();
  }

  function drawSamples(t) {
    data.region.sampleGrid.forEach((cell) => {
      ctx.beginPath();
      ctx.arc(t.x(cell.coord), t.y(cell.coord), cell.valid ? 2.5 : 1.5, 0, Math.PI * 2);
      ctx.fillStyle = cell.valid ? "#e6edf3" : "#5b6672";
      if (cell.inNoCollect) ctx.fillStyle = "#ff6b6b";
      ctx.fill();
    });
  }

  function drawTruth(t) {
    data.region.truth.forEach((tp) => {
      ctx.beginPath();
      ctx.arc(t.x(tp.coord), t.y(tp.coord), 2, 0, Math.PI * 2);
      ctx.fillStyle = rarityColors[tp.rarity];
      ctx.fill();
    });
  }

  function drawRecon(t) {
    data.report.results.forEach((res) => {
      res.points.forEach((p) => {
        ctx.beginPath();
        ctx.arc(t.x(p.coord), t.y(p.coord), 3, 0, Math.PI * 2);
        ctx.strokeStyle = rarityColors[p.rarity];
        ctx.lineWidth = 1.5;
        ctx.stroke();
      });
    });
  }

  function drawHeat(t) {
    let max = 0;
    data.report.results.forEach((r) =>
      r.heatTiles.forEach((tile) => {
        if (tile.density > max) max = tile.density;
      })
    );
    if (max === 0) max = 1;
    const cell = data.region.rasterCell * t.s;
    data.report.results.forEach((r) =>
      r.heatTiles.forEach((tile) => {
        const v = tile.density / max;
        ctx.fillStyle = heatColor(v);
        ctx.fillRect(t.x(tile.center) - cell / 2, t.y(tile.center) - cell / 2, cell + 1, cell + 1);
      })
    );
  }

  function heatColor(v) {
    const r = Math.round(40 + v * 215);
    const g = Math.round(60 + (1 - Math.abs(v - 0.5) * 2) * 120);
    const b = Math.round(220 - v * 180);
    return "rgba(" + r + "," + g + "," + b + "," + (0.25 + v * 0.65) + ")";
  }

  function clear() {
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    ctx.fillStyle = "#171f2a";
    ctx.fillRect(0, 0, canvas.width, canvas.height);
  }

  function draw() {
    if (!data) return;
    clear();
    const t = scale();
    drawZones(t);
    drawNoCollect(t);
    if (view === "overview") {
      drawTruth(t);
      sideOverview();
    } else if (view === "samples") {
      drawSamples(t);
      sideSamples();
    } else if (view === "points") {
      drawTruth(t);
      drawRecon(t);
      sidePoints();
    } else if (view === "heat") {
      drawHeat(t);
      sideHeat();
    } else if (view === "bounds") {
      drawBoundsChart();
      sideBounds();
      return;
    } else if (view === "invalidation") {
      drawSamples(t);
      sideInvalidation();
    }
  }

  function row(k, v, cls) {
    return '<div class="row"><span>' + k + '</span><span class="' + (cls || "") + '">' + v + "</span></div>";
  }

  function sideOverview() {
    sideTitle.textContent = "区域俯视与真值";
    let html = row("区域", data.region.name);
    html += row("栅格", data.region.rasterNx + "×" + data.region.rasterNy);
    html += row("地带数", data.region.zones.length);
    html += row("真值点数", data.region.truth.length);
    html += row("禁采区数", data.region.noCollect.length);
    sideBody.innerHTML = html;
  }

  function sideSamples() {
    sideTitle.textContent = "分层采样与禁采区";
    const valid = data.region.sampleGrid.filter((c) => c.valid).length;
    let html = row("采样格点", data.region.sampleGrid.length);
    html += row("有效格点", valid);
    html += row("观测记录", data.region.observations.length);
    html += row("量化步长", data.region.quantStep);
    sideBody.innerHTML = html;
  }

  function sidePoints() {
    sideTitle.textContent = "重建采集点";
    let html = '<table><tr><th>地带</th><th>重建</th><th>点摘要</th></tr>';
    data.report.results.forEach((r) => {
      html += "<tr><td>Z" + r.zoneId + "</td><td>" + r.points.length +
        '</td><td style="font-size:10px">' + r.pointDigest.slice(0, 10) + "…</td></tr>";
    });
    html += "</table>";
    sideBody.innerHTML = html;
  }

  function sideHeat() {
    sideTitle.textContent = "密度热力面";
    let html = row("热力瓦片", data.report.results.reduce((n, r) => n + r.heatTiles.length, 0));
    if (data.checks) {
      data.checks.forEach((c) => {
        html += row("Z" + c.zoneId + " 最大热力误差", c.maxHeatError.toFixed(4),
          c.heatWithin ? "pill ok" : "pill bad");
      });
    }
    sideBody.innerHTML = html;
  }

  function drawBoundsChart() {
    const records = data.report.bounds;
    if (!records || !records.length) return;
    const pad = 60;
    const w = canvas.width - pad * 2;
    const h = canvas.height - pad * 2;
    const series = [
      ["cumQuantBound", "#4cc2ff"],
      ["cumStdBound", "#7ee787"],
      ["cumHeatBound", "#ffb454"],
    ];
    let max = 0;
    records.forEach((r) =>
      series.forEach(([k]) => {
        if (r[k] > max) max = r[k];
      })
    );
    ctx.strokeStyle = "#2c3744";
    ctx.lineWidth = 1;
    for (let g = 0; g <= 4; g++) {
      const y = pad + (h * g) / 4;
      ctx.beginPath();
      ctx.moveTo(pad, y);
      ctx.lineTo(pad + w, y);
      ctx.stroke();
    }
    series.forEach(([k, color]) => {
      ctx.beginPath();
      ctx.strokeStyle = color;
      ctx.lineWidth = 2;
      records.forEach((r, i) => {
        const x = pad + (w * i) / Math.max(1, records.length - 1);
        const y = pad + h - (r[k] / max) * h;
        if (i === 0) ctx.moveTo(x, y);
        else ctx.lineTo(x, y);
      });
      ctx.stroke();
    });
    ctx.fillStyle = "#8b98a5";
    ctx.font = "12px sans-serif";
    ctx.fillText("蓝=量化上界  绿=标准误上界  橙=热力误差上界", pad, pad - 20);
    ctx.fillText("稀有度取整残差(累积, 整数毫单位): " + records[records.length - 1].cumRarityBound, pad, pad + h + 30);
  }

  function sideBounds() {
    sideTitle.textContent = "偏差上界报告";
    let html = '<table><tr><th>Z</th><th>量化</th><th>标准误</th><th>残差</th><th>热力</th></tr>';
    data.report.bounds.forEach((b) => {
      html += "<tr><td>Z" + b.zoneId + "</td><td>" + b.quantErrorBound.toFixed(4) +
        "</td><td>" + b.stdErrorBound.toFixed(4) + "</td><td>" + b.rarityResidual +
        "</td><td>" + b.heatErrorBound.toFixed(4) + "</td></tr>";
    });
    html += "</table>";
    sideBody.innerHTML = html;
  }

  function sideInvalidation() {
    sideTitle.textContent = "失效传播清单";
    let html = '<table><tr><th>地带</th><th>状态</th><th>原因</th></tr>';
    data.report.invalidations.forEach((e) => {
      html += "<tr><td>Z" + e.zoneId + '</td><td><span class="pill ' +
        (e.reused ? "ok" : "warn") + '">' + (e.reused ? "复用" : "失效") +
        "</span></td><td style='text-align:left;font-size:11px'>" + e.reason + "</td></tr>";
    });
    html += "</table>";
    if (data.queueLog && data.queueLog.length) {
      html += "<h3>缓冲排队</h3>";
      data.queueLog.forEach((q) => {
        html += row("Z" + q.zoneId, q.reason, "pill warn");
      });
    }
    sideBody.innerHTML = html;
  }
})();
