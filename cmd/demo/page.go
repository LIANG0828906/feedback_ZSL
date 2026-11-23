package main

// indexHTML is the single-page demo UI. It is intentionally
// dependency-free (no CDN, no build step).
const indexHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>captionx demo</title>
<style>
 body { font-family: system-ui, sans-serif; margin: 16px; background:#14171c; color:#dde3ea; }
 h1 { font-size: 18px; } h2 { font-size: 14px; margin: 12px 0 4px; color:#9fb2c8; }
 canvas { background:#1c2129; border:1px solid #2c3542; border-radius:6px; display:block; }
 button { background:#2b6cb0; color:#fff; border:0; border-radius:4px; padding:6px 12px; margin-right:6px; cursor:pointer; }
 button:hover { background:#3182ce; }
 input { width:64px; background:#0f1216; color:#dde3ea; border:1px solid #2c3542; border-radius:4px; padding:4px; }
 .row { display:flex; gap:16px; flex-wrap:wrap; align-items:flex-start; }
 .chunk { display:inline-block; padding:2px 8px; margin:2px; border-radius:4px; font-size:12px; }
 .pending { background:#3a4350; } .processing { background:#b7791f; }
 .committed { background:#2f855a; } .failed { background:#c53030; }
 #caption { font-size:28px; margin:12px 0; min-height:40px; letter-spacing:2px; }
 #caption .on { color:#f6e05e; background:#4a5568; border-radius:4px; }
 #notice { color:#68d391; min-height:18px; font-size:13px; }
 #decisions { font-size:12px; color:#9fb2c8; max-height:120px; overflow:auto; }
 label { font-size:12px; color:#9fb2c8; margin-right:4px; }
</style>
</head>
<body>
<h1>captionx 字幕排版演示 <span id="pid"></span></h1>
<div class="row">
 <div>
  <button onclick="step()">提交一段</button>
  <button onclick="runAll()">运行到完成</button>
  <button onclick="reset()">作废检查点</button>
  <button onclick="togglePlay()" id="playbtn">回放</button>
 </div>
 <div>
  <label>语速(字/秒)</label><input id="rateNum" value="6">/<input id="rateDen" value="1">
  <label>权重缩放</label><input id="wscale" value="1.0">
  <button onclick="applyParams()">应用并重算未提交分段</button>
 </div>
</div>
<div id="notice"></div>
<h2>分段状态 <span id="progress"></span></h2>
<div id="chunks"></div>
<h2>台词时间轴（灰=语音段，蓝=显示窗口，红=回放光标）</h2>
<canvas id="tl" width="1100" height="180"></canvas>
<div id="caption"></div>
<h2>误差传播曲线（实线=累计实测，虚线=累计上界）</h2>
<canvas id="err" width="1100" height="200"></canvas>
<h2>取舍决策日志</h2>
<div id="decisions"></div>
<script>
var S = null, playing = false, playT = 0, raf = null;
function post(u, body) {
 return fetch(u, {method:"POST", headers:{"Content-Type":"application/json"},
   body: body ? JSON.stringify(body) : null}).then(function(r){return r.json();}).then(render);
}
function refresh() { return fetch("/api/state").then(function(r){return r.json();}).then(render); }
function step() { post("/api/step"); }
function runAll() { post("/api/run"); }
function reset() { post("/api/reset"); }
function applyParams() {
 post("/api/params", {speechRateNum:+document.getElementById("rateNum").value,
   speechRateDen:+document.getElementById("rateDen").value,
   weightScale:+document.getElementById("wscale").value});
}
function render(st) {
 S = st;
 document.getElementById("pid").textContent = "(" + st.projectId + ")";
 document.getElementById("notice").textContent = st.notice || "";
 document.getElementById("progress").textContent = st.committed + "/" + st.total;
 var ch = document.getElementById("chunks"); ch.innerHTML = "";
 st.chunks.forEach(function(c, i) {
   var d = document.createElement("span"); d.className = "chunk " + c;
   d.textContent = i + ":" + c; ch.appendChild(d);
 });
 var dec = document.getElementById("decisions");
 dec.innerHTML = (st.decisions||[]).map(function(d){
   return "台词 " + d.captionId + " [" + d.kind + "] " + d.reason;
 }).join("<br>") || "（无）";
 drawTimeline(); drawErrors();
}
function x(t, W) { return 10 + (W-20) * t / S.totalSamples; }
function drawTimeline() {
 var cv = document.getElementById("tl"), ctx = cv.getContext("2d"), W = cv.width;
 ctx.clearRect(0,0,W,cv.height);
 ctx.fillStyle = "#3a4350";
 S.segments.forEach(function(s){ ctx.fillRect(x(s.start,W), 20, x(s.end,W)-x(s.start,W), 24); });
 S.chunks.forEach(function(c, i) {
   var t0 = i * S.chunkSamples;
   ctx.strokeStyle = "#2c3542";
   ctx.beginPath(); ctx.moveTo(x(t0,W), 10); ctx.lineTo(x(t0,W), 170); ctx.stroke();
 });
 (S.captions||[]).forEach(function(c, i) {
   if (!c.window) return;
   ctx.fillStyle = c.frozen ? "#2f855a" : "#2b6cb0";
   var y = 60 + (i%3)*30;
   ctx.fillRect(x(c.window.start,W), y, Math.max(2, x(c.window.end,W)-x(c.window.start,W)), 18);
   ctx.fillStyle = "#dde3ea"; ctx.font = "10px sans-serif";
   ctx.fillText("#" + c.id, x(c.window.start,W)+2, y+12);
 });
 if (playing || playT > 0) {
   ctx.strokeStyle = "#e53e3e"; ctx.beginPath();
   ctx.moveTo(x(playT,W), 10); ctx.lineTo(x(playT,W), 170); ctx.stroke();
 }
}
function drawErrors() {
 var cv = document.getElementById("err"), ctx = cv.getContext("2d"), W = cv.width, H = cv.height;
 ctx.clearRect(0,0,W,H);
 if (!S.report || !S.report.captions) return;
 var n = S.report.captions.length;
 var measW = [], measS = [], cw = 0, cs = 0;
 S.report.captions.forEach(function(t){ cw += t.windowDeviation; cs += t.schedResidual; measW.push(cw); measS.push(cs); });
 var boundW = S.report.windowDeviationBound, boundS = S.report.schedResidualBound;
 var maxV = 1;
 [measW, measS, boundW, boundS].forEach(function(a){ a.forEach(function(v){ if (v>maxV) maxV=v; }); });
 function plot(arr, color, dash) {
   ctx.strokeStyle = color; ctx.setLineDash(dash ? [4,3] : []);
   ctx.beginPath();
   arr.forEach(function(v, i) {
     var px = 10 + (W-20) * (i+1) / n, py = H-10 - (H-30) * v / maxV;
     if (i === 0) ctx.moveTo(px, py); else ctx.lineTo(px, py);
   });
   ctx.stroke();
 }
 plot(measW, "#63b3ed", false); plot(boundW, "#63b3ed", true);
 plot(measS, "#f6ad55", false); plot(boundS, "#f6ad55", true);
 ctx.setLineDash([]); ctx.fillStyle = "#9fb2c8"; ctx.font = "11px sans-serif";
 ctx.fillText("蓝=窗口推导偏差  橙=排程取整残差", 12, 14);
}
function togglePlay() {
 if (!S || !S.captions || !S.captions[0].window) return;
 playing = !playing;
 document.getElementById("playbtn").textContent = playing ? "暂停" : "回放";
 if (playing) { if (playT >= S.totalSamples) playT = 0; tick(); }
}
function tick() {
 if (!playing) return;
 playT += S.frame.sampleRate / 30;
 if (playT >= S.totalSamples) { playing = false; document.getElementById("playbtn").textContent = "回放"; }
 drawTimeline(); drawCaption();
 raf = setTimeout(tick, 33);
}
function drawCaption() {
 var el = document.getElementById("caption"); el.innerHTML = "";
 (S.captions||[]).forEach(function(c) {
   if (!c.window || playT < c.window.start || playT >= c.window.end) return;
   for (var i = 0; i < c.text.length; i++) {
     var sp = document.createElement("span");
     sp.textContent = c.text[i];
     if (c.charStarts && playT >= c.charStarts[i] && playT < c.charEnds[i]) sp.className = "on";
     el.appendChild(sp);
   }
 });
}
refresh();
</script>
</body>
</html>`
