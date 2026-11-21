from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, urlparse

import numpy as np

from .coordinator import SceneRuntime
from .errors import ProbeError
from .sh import evaluate_irradiance, real_sh_basis

HTML = r"""<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Local Light Probe Baker</title>
<style>
:root{--bg:#10151b;--panel:#18212b;--line:#2f3d4b;--text:#dce7f2;--muted:#8ea2b5;--accent:#5cc8ff;--warn:#ffb45c;--bad:#ff6b6b}
*{box-sizing:border-box}html,body{margin:0;height:100%;overflow:hidden;background:var(--bg);color:var(--text);font:13px/1.45 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
#view{position:absolute;inset:0;width:100%;height:100%}
#panel{position:absolute;top:14px;left:14px;width:322px;max-height:calc(100% - 28px);overflow:auto;background:color-mix(in srgb,var(--panel) 93%,transparent);border:1px solid var(--line);border-radius:10px;padding:14px;box-shadow:0 12px 40px #0008;backdrop-filter:blur(8px)}
h1{font-size:16px;margin:0 0 4px;letter-spacing:.02em}.sub{color:var(--muted);font-size:11px;margin-bottom:12px}
.row{display:grid;grid-template-columns:1fr 92px;gap:8px;align-items:center;margin:7px 0}
.row.wide{grid-template-columns:1fr}.row label{color:var(--muted)}
input,select,button,textarea{background:#0d1319;border:1px solid var(--line);color:var(--text);border-radius:6px;padding:6px 8px;font:inherit;min-width:0}
input[type=range]{padding:0;accent-color:var(--accent)}button{cursor:pointer}button:hover{border-color:var(--accent)}
button.primary{background:#134b68;border-color:#2d86b0;font-weight:700}button.danger{background:#512126;border-color:#8f3b45}
.buttons{display:grid;grid-template-columns:1fr 1fr;gap:8px;margin:12px 0}
.sep{height:1px;background:var(--line);margin:12px 0}.meta{font-size:11px;color:var(--muted);white-space:pre-wrap;word-break:break-word}
#status{padding:8px;border:1px solid var(--line);border-radius:6px;background:#0b1015;min-height:54px;white-space:pre-wrap}
#status.bad{border-color:#7f3038;color:#ffb7bd}#status.ok{border-color:#2d6654}
.legend{display:flex;height:8px;border-radius:4px;overflow:hidden;margin:5px 0}.legend div{flex:1}
.tag{display:inline-block;padding:1px 5px;border:1px solid var(--line);border-radius:4px;color:var(--muted);margin-right:4px}
#tooltip{position:absolute;pointer-events:none;padding:4px 7px;background:#000c;border:1px solid var(--line);border-radius:4px;display:none;font-size:11px}
</style>
</head>
<body>
<canvas id="view"></canvas><div id="tooltip"></div>
<div id="panel">
  <h1>Local Light Probe Baker</h1>
  <div class="sub">two-level deterministic probe bake / immutable versions</div>
  <div id="status">loading…</div>
  <div class="sep"></div>
  <div class="row"><label>探针间距</label><input id="spacing" type="number" min="0.25" step="0.25" value="2"></div>
  <div class="row"><label>球谐阶数</label><select id="order"><option>2</option><option>3</option><option selected>4</option><option>5</option><option>6</option></select></div>
  <div class="row"><label>补偿强度</label><input id="comp" type="range" min="0" max="1.5" step="0.05" value="0.4"><span id="compValue">0.40</span></div>
  <div class="sep"></div>
  <div class="row wide"><label>遮挡体</label><select id="object"></select></div>
  <div class="row"><label>位移 X</label><input id="dx" type="number" step="0.25" value="0"></div>
  <div class="row"><label>位移 Y</label><input id="dy" type="number" step="0.25" value="0"></div>
  <div class="row"><label>位移 Z</label><input id="dz" type="number" step="0.25" value="0"></div>
  <div class="buttons"><button id="bake" class="primary">启动重烘</button><button id="cancel" class="danger">取消</button></div>
  <div class="buttons"><button id="query">查询中心辐照度</button><button id="toggleProbes">隐藏探针</button></div>
  <div class="row"><label>拖动旋转 / 滚轮缩放</label><label id="zoom">1.00×</label></div>
  <div class="sep"></div>
  <div class="meta" id="meta"></div>
  <div class="legend"><div style="background:#132a5c"></div><div style="background:#176d86"></div><div style="background:#29a37a"></div><div style="background:#c7c849"></div><div style="background:#e98738"></div><div style="background:#dc3f3f"></div></div>
</div>
<script>
const canvas=document.getElementById('view'), ctx=canvas.getContext('2d');
const tooltip=document.getElementById('tooltip');
let state=null, yaw=-0.72, pitch=0.48, zoom=1, drag=null, showProbes=true, lastStateAt=0;
let lastQuery=null;
const $=id=>document.getElementById(id);
function fmt(v){return Number(v).toFixed(3)}
function heat(t){t=Math.max(0,Math.min(1,t));const stops=[[19,42,92],[23,109,134],[41,163,122],[199,200,73],[233,135,56],[220,63,63]];const x=t*5,i=Math.min(4,Math.floor(x)),f=x-i,a=stops[i],b=stops[i+1];return `rgb(${a.map((v,j)=>Math.round(v+(b[j]-v)*f)).join(',')})`}
function resize(){canvas.width=innerWidth*devicePixelRatio;canvas.height=innerHeight*devicePixelRatio;canvas.style.width=innerWidth+'px';canvas.style.height=innerHeight+'px';ctx.setTransform(devicePixelRatio,0,0,devicePixelRatio,0,0);draw()}
addEventListener('resize',resize);
function rot(p){const cy=Math.cos(yaw),sy=Math.sin(yaw),cp=Math.cos(pitch),sp=Math.sin(pitch);let x=p[0]*cy+p[2]*sy,z=-p[0]*sy+p[2]*cy;let y=p[1]*cp-z*sp;z=p[1]*sp+z*cp;return [x,y,z]}
function project(p,center,radius){const q=rot([p[0]-center[0],p[1]-center[1],p[2]-center[2]]);const d=radius*3.2;const f=Math.min(innerWidth,innerHeight)*0.9*zoom;const z=d-q[2];if(z<=0.05)return null;const s=f/z;return [innerWidth/2+q[0]*s,innerHeight/2-q[1]*s,s]}
function line(a,b,color,width=1){if(!a||!b)return;ctx.strokeStyle=color;ctx.lineWidth=width;ctx.beginPath();ctx.moveTo(a[0],a[1]);ctx.lineTo(b[0],b[1]);ctx.stroke()}
function drawBox(o,center,radius){const mn=o.min,mx=o.max,c=[];for(const x of [mn[0],mx[0]])for(const y of [mn[1],mx[1]])for(const z of [mn[2],mx[2]])c.push(project([x,y,z],center,radius));const edges=[[0,1],[0,2],[0,4],[1,3],[1,5],[2,3],[2,6],[3,7],[4,5],[4,6],[5,7],[6,7]];for(const [a,b] of edges)line(c[a],c[b],'#7890a6aa',1.2)}
function drawSphere(o,center,radius){const r=o.radius, pts=[];for(let i=0;i<=16;i++){const a=i/16*Math.PI*2;pts.push(project([o.center[0]+r*Math.cos(a),o.center[1],o.center[2]+r*Math.sin(a)],center,radius))}for(let i=1;i<pts.length;i++)line(pts[i-1],pts[i],'#8aa0b4aa',1);pts.length=0;for(let i=0;i<=16;i++){const a=i/16*Math.PI*2;pts.push(project([o.center[0],o.center[1]+r*Math.cos(a),o.center[2]+r*Math.sin(a)],center,radius))}for(let i=1;i<pts.length;i++)line(pts[i-1],pts[i],'#8aa0b4aa',1)}
function draw(){ctx.clearRect(0,0,innerWidth,innerHeight);const g=ctx.createRadialGradient(innerWidth*.55,innerHeight*.4,10,innerWidth*.55,innerHeight*.4,Math.max(innerWidth,innerHeight)*.75);g.addColorStop(0,'#17222d');g.addColorStop(1,'#090d12');ctx.fillStyle=g;ctx.fillRect(0,0,innerWidth,innerHeight);if(!state||!state.scene)return;const b=state.scene.bounds,center=[(b.min[0]+b.max[0])/2,(b.min[1]+b.max[1])/2,(b.min[2]+b.max[2])/2];const radius=Math.max(1,Math.hypot(b.max[0]-b.min[0],b.max[1]-b.min[1],b.max[2]-b.min[2])/2);const bc=[];for(const x of [b.min[0],b.max[0]])for(const y of [b.min[1],b.max[1]])for(const z of [b.min[2],b.max[2]])bc.push(project([x,y,z],center,radius));const edges=[[0,1],[0,2],[0,4],[1,3],[1,5],[2,3],[2,6],[3,7],[4,5],[4,6],[5,7],[6,7]];for(const [a,c] of edges)line(bc[a],bc[c],'#314658',1);for(const o of state.scene.occluders){if(o.type==='aabb')drawBox(o,center,radius);else drawSphere(o,center,radius)}if(showProbes&&state.probes){let max=1e-6;for(const p of state.probes)max=Math.max(max,p.heat);for(const p of state.probes){const q=project(p.position,center,radius);if(!q)continue;ctx.fillStyle=heat(p.heat/max);ctx.globalAlpha=.82;ctx.beginPath();ctx.arc(q[0],q[1],Math.max(1.5,Math.min(4.5,q[2]/radius*1.8)),0,Math.PI*2);ctx.fill()}ctx.globalAlpha=1}if(lastQuery&&lastQuery.position){const q=project(lastQuery.position,center,radius);if(q){ctx.strokeStyle='#ffffff';ctx.lineWidth=1.5;ctx.beginPath();ctx.arc(q[0],q[1],8,0,Math.PI*2);ctx.stroke();ctx.fillStyle='#fff';ctx.font='11px ui-monospace';ctx.fillText(`E ${lastQuery.irradiance.map(fmt).join(' / ')}`,q[0]+12,q[1]-8)}}}
canvas.addEventListener('pointerdown',e=>{drag={x:e.clientX,y:e.clientY};canvas.setPointerCapture(e.pointerId)});canvas.addEventListener('pointermove',e=>{if(!drag)return;yaw+=(e.clientX-drag.x)*.008;pitch+=(e.clientY-drag.y)*.008;pitch=Math.max(-1.45,Math.min(1.45,pitch));drag={x:e.clientX,y:e.clientY};draw()});canvas.addEventListener('pointerup',()=>drag=null);canvas.addEventListener('wheel',e=>{e.preventDefault();zoom*=Math.exp(-e.deltaY*.001);zoom=Math.max(.25,Math.min(8,zoom));$('zoom').textContent=zoom.toFixed(2)+'×';draw()},{passive:false});
function setStatus(text,kind=''){const el=$('status');el.textContent=text;el.className=kind}
function currentObject(){const o=state?.scene?.occluders?.find(x=>x.id===$('object').value);return o}
function populate(){if(!state||!state.scene)return;const sel=$('object');const old=sel.value;sel.innerHTML='';for(const o of state.scene.occluders){const opt=document.createElement('option');opt.value=o.id;opt.textContent=`${o.id} (${o.type})`;sel.appendChild(opt)}if(old)sel.value=old;$('spacing').value=state.scene.settings.probe_spacing;$('order').value=state.scene.settings.sh_order;$('comp').value=state.scene.settings.compensation_strength;$('compValue').textContent=Number($('comp').value).toFixed(2);if(state.published){$('meta').textContent=`version ${state.published.version}\nhash ${state.published.content_hash.slice(0,24)}…\nstrategy ${state.published.strategy}\nprobes ${state.published.probe_count}\nspacing ${state.published.spacing}`}}
async function refresh(){try{const r=await fetch('/api/state',{cache:'no-store'});const j=await r.json();if(!j.ok)throw new Error(j.message||'state failed');state=j;lastStateAt=Date.now();populate();let txt='';let kind='';if(j.busy){txt=`running ${j.phase.phase} ${j.run_id?.slice(0,8)||''}\n`;if(j.phase.completed!==undefined)txt+=`${j.phase.completed}/${j.phase.total}`;}else if(j.last_error){txt=`${j.last_error.code}: ${j.last_error.message}`;kind='bad'}else if(j.published){txt=`ready · version ${j.published.version} · ${j.published.strategy}`;kind='ok'}else{txt='no published version';}if(j.last_report&&!j.busy)txt+=`\nreused ${j.last_report.reused_count} / recomputed ${j.last_report.recomputed_count}\nmax Δcoef ${Number(j.last_report.max_coefficient_deviation).toExponential(2)} · trunc ${Number(j.last_report.max_recomputed_truncation_error).toFixed(3)}`;setStatus(txt,kind);draw();if(j.busy)setTimeout(refresh,350)}catch(e){setStatus(String(e),'bad');setTimeout(refresh,1500)}}
$('comp').addEventListener('input',()=>{$('compValue').textContent=Number($('comp').value).toFixed(2)});
$('toggleProbes').onclick=()=>{showProbes=!showProbes;$('toggleProbes').textContent=showProbes?'隐藏探针':'显示探针';draw()};
$('bake').onclick=async()=>{let updates={settings:{probe_spacing:Number($('spacing').value),sh_order:Number($('order').value),compensation_strength:Number($('comp').value)}};const o=currentObject(),dx=Number($('dx').value),dy=Number($('dy').value),dz=Number($('dz').value);if(o&&(dx||dy||dz)){const moved=JSON.parse(JSON.stringify(o));if(o.type==='aabb'){moved.min=o.min.map((v,i)=>v+[dx,dy,dz][i]);moved.max=o.max.map((v,i)=>v+[dx,dy,dz][i])}else{moved.center=o.center.map((v,i)=>v+[dx,dy,dz][i])}updates.occluders={[o.id]:moved}}await post('/api/bake',updates)};
$('cancel').onclick=()=>post('/api/cancel',{});$('query').onclick=async()=>{if(!state?.published)return;const b=state.scene.bounds,pos=[0,1,2].map(i=>(b.min[i]+b.max[i])/2);const j=await post('/api/query',{position:pos,expected_version:state.published.version,expected_content_hash:state.published.content_hash});if(j?.ok){lastQuery=j;setStatus(`query v${j.version}\nE ${j.irradiance.map(fmt).join(' / ')}\nprobes ${j.probes.length} all v${j.version}`, 'ok');draw()}};
async function post(path,body){try{const r=await fetch(path,{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify(body)});const j=await r.json();if(!j.ok)setStatus(`${j.code}: ${j.message}`,'bad');else if(path==='/api/bake')setStatus(`started ${j.run_id.slice(0,8)}`,'ok');if(path==='/api/bake'||path==='/api/cancel')setTimeout(refresh,150);return j}catch(e){setStatus(String(e),'bad');return null}}
resize();refresh();setInterval(()=>{if(Date.now()-lastStateAt>3000)refresh()},4000);
</script>
</body>
</html>
"""


def _probe_payload(runtime: SceneRuntime) -> list[dict[str, Any]]:
    published = runtime.store.current
    if published is None:
        return []
    state = published.state
    up = np.asarray([[0.0, 1.0, 0.0]], dtype=np.float64)
    basis = real_sh_basis(up, state.config.sh_order)[0]
    probes = []
    for probe_id in state.grid.probe_ids:
        record = state.records[probe_id]
        irradiance = record.coefficients.T @ basis
        heat = max(0.0, float(np.dot(irradiance, (0.2126, 0.7152, 0.0722))))
        probes.append(
            {
                "probe_id": probe_id,
                "coordinate_hash": state.grid.coordinate_hashes[
                    state.grid.index_by_id[probe_id]
                ],
                "position": [
                    float(item) for item in state.grid.position(probe_id)
                ],
                "heat": heat,
                "irradiance": [float(item) for item in irradiance],
                "leak_score": record.leak_score,
                "truncation_error": record.truncation_error,
                "content_hash": record.content_hash,
            }
        )
    return probes


class _Handler(BaseHTTPRequestHandler):
    server: "_HTTPServer"

    def log_message(self, format: str, *args: Any) -> None:
        return

    def _send_json(self, payload: dict[str, Any], status: int = 200) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("content-type", "application/json; charset=utf-8")
        self.send_header("content-length", str(len(body)))
        self.send_header("cache-control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def _send_html(self) -> None:
        body = HTML.encode("utf-8")
        self.send_response(200)
        self.send_header("content-type", "text/html; charset=utf-8")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read_json(self) -> dict[str, Any]:
        length = int(self.headers.get("content-length", "0"))
        if length > 1_000_000:
            raise ProbeError("E_SCENE_INVALID", "request body is too large")
        if length == 0:
            return {}
        payload = json.loads(self.rfile.read(length).decode("utf-8"))
        if not isinstance(payload, dict):
            raise ProbeError("E_SCENE_INVALID", "request body must be a JSON object")
        return payload

    def do_GET(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        if parsed.path == "/":
            self._send_html()
            return
        if parsed.path == "/api/state":
            payload = self.server.runtime.status()
            payload["probes"] = _probe_payload(self.server.runtime)
            self._send_json(payload)
            return
        if parsed.path == "/api/report":
            status = self.server.runtime.status()
            self._send_json(
                {"ok": True, "report": status["last_report"], "phase": status["phase"]}
            )
            return
        if parsed.path == "/api/query":
            query = parse_qs(parsed.query)
            try:
                position = [
                    float(query.get("x", ["0"])[0]),
                    float(query.get("y", ["0"])[0]),
                    float(query.get("z", ["0"])[0]),
                ]
                result = self.server.runtime.query(position)
                self._send_json(result)
            except ProbeError as exc:
                self._send_json(exc.to_dict(), _status_for(exc))
            return
        self._send_json({"ok": False, "code": "E_NOT_FOUND", "message": "not found"}, 404)

    def do_POST(self) -> None:  # noqa: N802
        try:
            payload = self._read_json()
            if self.path == "/api/bake":
                updates = payload.get("updates", payload)
                overrides = payload.get("config_overrides", {})
                self._send_json(
                    self.server.runtime.start_bake(updates, config_overrides=overrides)
                )
                return
            if self.path == "/api/cancel":
                self._send_json(self.server.runtime.cancel())
                return
            if self.path == "/api/query":
                result = self.server.runtime.query(
                    payload["position"],
                    payload.get("normal"),
                    expected_version=payload.get("expected_version"),
                    expected_content_hash=payload.get("expected_content_hash"),
                )
                self._send_json(result)
                return
            if self.path == "/api/resume":
                self._send_json(
                    self.server.runtime.resume_snapshot(payload["snapshot_dir"])
                )
                return
            self._send_json(
                {"ok": False, "code": "E_NOT_FOUND", "message": "not found"}, 404
            )
        except ProbeError as exc:
            self._send_json(exc.to_dict(), _status_for(exc))
        except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
            self._send_json(
                {
                    "ok": False,
                    "code": "E_SCENE_INVALID",
                    "message": f"invalid request: {exc}",
                    "details": {"path": "$"},
                },
                400,
            )


def _status_for(error: ProbeError) -> int:
    if error.code in {"E_BUSY", "E_CANCELLED", "E_VERSION_STALE"}:
        return 409
    if error.code.startswith("E_SCENE") or error.code.startswith("E_SH"):
        return 400
    return 422


class _HTTPServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address: tuple[str, int], runtime: SceneRuntime) -> None:
        self.runtime = runtime
        super().__init__(address, _Handler)


def create_server(
    runtime: SceneRuntime, host: str = "127.0.0.1", port: int = 8765
) -> ThreadingHTTPServer:
    return _HTTPServer((host, port), runtime)


def serve(runtime: SceneRuntime, host: str = "127.0.0.1", port: int = 8765) -> None:
    server = create_server(runtime, host, port)
    print(f"Light probe UI: http://{host}:{server.server_port}/", flush=True)
    try:
        server.serve_forever()
    finally:
        server.server_close()


def serve_in_thread(
    runtime: SceneRuntime, host: str = "127.0.0.1", port: int = 0
) -> tuple[ThreadingHTTPServer, threading.Thread]:
    server = create_server(runtime, host, port)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, thread

