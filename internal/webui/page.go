package webui

const pageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Skeleton Retarget Diagnostics</title>
<style>
:root{color-scheme:dark;--bg:#0d1117;--panel:#161b22;--line:#30363d;--text:#e6edf3;--muted:#8b949e;--accent:#58a6ff;--ok:#3fb950;--bad:#f85149;--warn:#d29922}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace}
header{padding:24px 28px;border-bottom:1px solid var(--line);background:linear-gradient(120deg,#111827,#161b22)}
h1{margin:0 0 6px;font-size:24px}h2{margin:0 0 12px;font-size:16px;color:var(--accent)}
.sub{color:var(--muted)}main{padding:22px 28px;max-width:1500px;margin:auto}.grid{display:grid;grid-template-columns:repeat(12,1fr);gap:16px}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:8px;padding:16px;overflow:auto}
.span4{grid-column:span 4}.span6{grid-column:span 6}.span8{grid-column:span 8}.span12{grid-column:span 12}
@media(max-width:1000px){.span4,.span6,.span8{grid-column:span 12}}
table{width:100%;border-collapse:collapse;font-size:12px}th,td{padding:6px 8px;border-bottom:1px solid var(--line);text-align:left;vertical-align:top}th{color:var(--muted);font-weight:600}
.ok{color:var(--ok)}.bad{color:var(--bad)}.warn{color:var(--warn)}.muted{color:var(--muted)}
.hierarchy{white-space:pre;line-height:1.6}.toolbar{display:flex;gap:12px;align-items:center;margin-bottom:12px}
select,button{background:#21262d;color:var(--text);border:1px solid var(--line);border-radius:6px;padding:6px 9px;font:inherit}
canvas{width:100%;height:280px}.legend{display:flex;gap:18px;flex-wrap:wrap;margin-top:8px;color:var(--muted);font-size:12px}
.dot{display:inline-block;width:9px;height:9px;border-radius:50%;margin-right:5px}
</style>
</head>
<body>
<header>
  <h1>Skeleton Retarget Diagnostics</h1>
  <div class="sub" id="subtitle"></div>
</header>
<main>
<div class="grid">
  <section class="panel span8"><h2>Release Formats</h2><table id="formats"></table></section>
  <section class="panel span4"><h2>Path Comparison</h2><table id="comparisons"></table></section>
  <section class="panel span4"><h2>Source Hierarchy</h2><div class="hierarchy" id="hierarchy"></div></section>
  <section class="panel span4"><h2>Target Proportions</h2><table id="targets"></table></section>
  <section class="panel span4"><h2>Retarget Mapping</h2><table id="mappings"></table></section>
  <section class="panel span8"><h2>Residual And Upper-Bound Curves</h2>
    <canvas id="curve" width="1000" height="300"></canvas>
    <div class="legend">
      <span><i class="dot" style="background:#f85149"></i>actual sample deviation</span>
      <span><i class="dot" style="background:#58a6ff"></i>derived upper bound</span>
      <span><i class="dot" style="background:#3fb950"></i>normalization residual</span>
    </div>
  </section>
  <section class="panel span4"><h2>Run Paths</h2><table id="paths"></table></section>
  <section class="panel span12"><h2>Invalidation, Cache Hits And Edit Comparison</h2><table id="edit"></table></section>
  <section class="panel span12"><h2>Per-Frame Target Joint Pose</h2>
    <div class="toolbar"><label for="frame">frame</label><select id="frame"></select><span class="muted" id="frameMeta"></span></div>
    <table id="poses"></table>
  </section>
</div>
</main>
<script id="report" type="application/json">__REPORT_JSON__</script>
<script>
var data = JSON.parse(document.getElementById('report').textContent);
function byId(id) { return document.getElementById(id); }
function fmt(value) { return new Intl.NumberFormat().format(value); }
function table(node, headers, rows) {
  var html = '<thead><tr>';
  headers.forEach(function(h) { html += '<th>' + h + '</th>'; });
  html += '</tr></thead><tbody>';
  rows.forEach(function(row) {
    html += '<tr>';
    row.forEach(function(cell) { html += '<td>' + cell + '</td>'; });
    html += '</tr>';
  });
  node.innerHTML = html + '</tbody>';
}
byId('subtitle').textContent = 'seed ' + data.seed + ' · algorithm ' + data.algorithmVersion + ' · interval size ' + data.intervalSize;
table(byId('formats'), ['name','source version','action hash','frames','joints','conversions'],
  data.formats.map(function(f) {
    return [f.name, f.sourceVersion, '<span class="muted">' + f.actionHash.slice(0,16) + '…</span>', f.frames, f.sourceJoints + '→' + f.targetJoints, f.conversions];
  }));
table(byId('comparisons'), ['left','right','result','mismatches'],
  data.comparisons.map(function(c) {
    var mismatch = (c.mismatches || []).slice(0, 3).join('<br>') || '—';
    return [c.left, c.right, c.equal ? '<span class="ok">equal</span>' : '<span class="bad">different</span>', mismatch];
  }));
table(byId('targets'), ['joint','parent','rest','proportion'],
  data.targetSkeleton.map(function(j) { return [j.id, j.parent || 'root', j.rest, j.proportion]; }));
table(byId('mappings'), ['id','source','target','rotation','translation'],
  data.mappings.map(function(m) { return [m.id, m.source, m.target, m.rotationScale, m.translationScale]; }));
table(byId('paths'), ['path','frames','hits','recomputed','inval.','recovered','digest'],
  data.paths.map(function(p) {
    return [p.name, p.frames, p.cacheHits, p.recomputed, p.invalidationCount, p.recoveredTo,
      '<span class="muted">' + p.digest.slice(0,12) + '…</span>'];
  }));
var editRows = [['edit', data.edit.description, data.edit.hits, data.edit.recomputed,
  data.edit.comparison.equal ? '<span class="ok">full = incremental</span>' : '<span class="bad">mismatch</span>']];
data.edit.invalidations.forEach(function(i) {
  editRows.push(['#' + i.intervalId, 'frames ' + i.frames.join(','), '—', '—',
    i.joints.join(', ') + '<br><span class="warn">' + i.reasons.join('; ') + '</span>']);
});
table(byId('edit'), ['interval','basis','cache hits','recomputed','affected joints'], editRows);
var children = {};
data.sourceSkeleton.forEach(function(j) {
  if (!children[j.parent]) children[j.parent] = [];
  children[j.parent].push(j);
});
function walk(parent, indent) {
  var text = '';
  (children[parent] || []).forEach(function(j) {
    text += indent + j.id + '  [' + j.minAngle + ' .. ' + j.maxAngle + ']\n';
    text += walk(j.id, indent + '  ');
  });
  return text;
}
byId('hierarchy').textContent = walk('', '');
var frameSelect = byId('frame');
data.frames.forEach(function(frame) {
  var option = document.createElement('option');
  option.value = frame.frame;
  option.textContent = 'frame ' + frame.frame + ' · ' + frame.timeMs + ' ms';
  frameSelect.appendChild(option);
});
function renderPose() {
  var frame = data.frames.find(function(f) { return f.frame === Number(frameSelect.value); }) || data.frames[0];
  if (!frame) return;
  byId('frameMeta').textContent = frame.joints.length + ' target joints';
  table(byId('poses'), ['joint','world quaternion (w,x,y,z)','local quaternion (w,x,y,z)','position (x,y,z)','bound ppm','actual ppm'],
    frame.joints.map(function(j) {
      var actual = j.report.actualDeviationPpm == null ? '<span class="muted">sampled only</span>' : fmt(j.report.actualDeviationPpm);
      return [j.joint,
        [j.worldRotation.W,j.worldRotation.X,j.worldRotation.Y,j.worldRotation.Z].join(', '),
        [j.localRotation.W,j.localRotation.X,j.localRotation.Y,j.localRotation.Z].join(', '),
        [j.position.X,j.position.Y,j.position.Z].join(', '),
        fmt(j.report.boundPpm), actual];
    }));
}
frameSelect.addEventListener('change', renderPose);
renderPose();
function drawCurve() {
  var canvas = byId('curve');
  var rect = canvas.getBoundingClientRect();
  var dpr = window.devicePixelRatio || 1;
  canvas.width = rect.width * dpr;
  canvas.height = rect.height * dpr;
  var ctx = canvas.getContext('2d');
  ctx.scale(dpr, dpr);
  var w = rect.width, h = rect.height, pad = 36;
  ctx.fillStyle = '#0d1117'; ctx.fillRect(0, 0, w, h);
  var points = data.curve;
  var maxY = 1;
  points.forEach(function(p) {
    maxY = Math.max(maxY, p.boundPpm, p.actualPpm || 0, p.normResidualPpm);
  });
  function x(i) { return pad + (w - 2 * pad) * (points.length <= 1 ? 0 : i / (points.length - 1)); }
  function y(v) { return h - pad - (h - 2 * pad) * v / maxY; }
  ctx.strokeStyle = '#30363d'; ctx.lineWidth = 1;
  for (var i = 0; i <= 4; i++) {
    var gy = pad + (h - 2 * pad) * i / 4;
    ctx.beginPath(); ctx.moveTo(pad, gy); ctx.lineTo(w - pad, gy); ctx.stroke();
  }
  ctx.fillStyle = '#8b949e'; ctx.font = '11px ui-monospace';
  ctx.fillText(fmt(maxY) + ' ppm', 8, pad - 12);
  ctx.fillText('frame ' + points[0].frame, pad, h - 10);
  ctx.fillText('frame ' + points[points.length - 1].frame, w - pad - 72, h - 10);
  function line(key, color, width) {
    ctx.strokeStyle = color; ctx.lineWidth = width; ctx.beginPath();
    var started = false;
    points.forEach(function(p, i) {
      if (p[key] == null) return;
      if (!started) { ctx.moveTo(x(i), y(p[key])); started = true; }
      else { ctx.lineTo(x(i), y(p[key])); }
    });
    ctx.stroke();
  }
  line('boundPpm', '#58a6ff', 2.2);
  line('normResidualPpm', '#3fb950', 1.6);
  line('actualPpm', '#f85149', 2);
  points.forEach(function(p, i) {
    if (p.actualPpm == null) return;
    ctx.fillStyle = '#f85149';
    ctx.beginPath(); ctx.arc(x(i), y(p.actualPpm), 3, 0, Math.PI * 2); ctx.fill();
  });
}
drawCurve();
window.addEventListener('resize', drawCurve);
</script>
</body>
</html>`
