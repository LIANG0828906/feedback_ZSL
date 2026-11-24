package dashboard

import (
	"encoding/json"
	"net/http"
	"os"

	"audiocheck/internal/model"
)

type App struct {
	ReportPath string
	Report     *model.Report
}

func NewServer(reportPath string) *App {
	return &App{ReportPath: reportPath}
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.index)
	mux.HandleFunc("/api/report", a.reportAPI)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func (a *App) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, a.Handler())
}

func (a *App) loadReport() (*model.Report, error) {
	if a.Report != nil {
		return a.Report, nil
	}
	b, err := os.ReadFile(a.ReportPath)
	if err != nil {
		return nil, err
	}
	var r model.Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (a *App) reportAPI(w http.ResponseWriter, r *http.Request) {
	report, err := a.loadReport()
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(report)
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}

const dashboardHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>Audio codec evaluation dashboard</title>
<style>
:root { color-scheme: dark; font-family: Inter, "Segoe UI", sans-serif; }
body { margin: 0; background: #0b1020; color: #e5e7eb; }
header { padding: 18px 24px; background: linear-gradient(120deg,#172554,#0f766e); box-shadow: 0 8px 30px #0006; }
h1 { margin: 0; font-size: 22px; } h2 { font-size: 16px; margin: 0 0 10px; color: #bfdbfe; }
main { padding: 20px; display: grid; grid-template-columns: repeat(2,minmax(360px,1fr)); gap: 16px; }
.card { background: #111827; border: 1px solid #243044; border-radius: 14px; padding: 14px; box-shadow: 0 10px 30px #0004; }
.wide { grid-column: 1 / -1; }
canvas { width: 100%; height: 240px; background: #0b1220; border-radius: 10px; }
table { width: 100%; border-collapse: collapse; font-size: 12px; }
th,td { padding: 6px 8px; border-bottom: 1px solid #243044; text-align: left; }
th { color: #93c5fd; }
.matrix { display: flex; flex-wrap: wrap; gap: 8px; }
.block { width: 118px; border-radius: 10px; padding: 8px; background: #1f2937; border: 1px solid #374151; font-size: 12px; }
.stage { display: inline-block; width: 10px; height: 10px; border-radius: 50%; margin-right: 4px; }
.pending { background: #64748b; } .processing { background: #f59e0b; } .committed { background: #22c55e; } .failed { background: #ef4444; }
.ok { color: #86efac; } .bad { color: #fca5a5; }
code { color: #a5f3fc; }
</style>
</head>
<body>
<header><h1>游戏音效编解码质量评估诊断台</h1><div id="meta">loading /api/report ...</div></header>
<main>
<section class="card"><h2>逐帧 PSNR / SNR</h2><canvas id="snrChart"></canvas></section>
<section class="card"><h2>SSIM</h2><canvas id="ssimChart"></canvas></section>
<section class="card"><h2>噪声基底与 RMS 上界</h2><canvas id="noiseChart"></canvas></section>
<section class="card"><h2>差异热力图（帧 × 帧内 32 带）</h2><canvas id="heatChart"></canvas></section>
<section class="card wide"><h2>块状态矩阵</h2><div id="matrix" class="matrix"></div></section>
<section class="card wide"><h2>预算与排队明细</h2><table id="queueTable"><thead><tr><th>块</th><th>原因</th><th>排队 ns</th><th>调度 ns</th><th>合计 ns</th><th>worker</th><th>队列深度</th><th>缓冲采样</th></tr></thead><tbody></tbody></table></section>
</main>
<script>
const scale = 1e6;
function db(v, inf) { return inf ? 180 : v / scale; }
function drawLine(id, series, yMin, yMax) {
  const c = document.getElementById(id), dpr = window.devicePixelRatio || 1;
  const w = c.clientWidth, h = c.clientHeight; c.width = w*dpr; c.height=h*dpr;
  const x = c.getContext('2d'); x.scale(dpr,dpr); x.clearRect(0,0,w,h);
  const pad=38, iw=w-pad*2, ih=h-pad*2;
  x.strokeStyle='#334155'; x.beginPath(); x.moveTo(pad,pad); x.lineTo(pad,h-pad); x.lineTo(w-pad,h-pad); x.stroke();
  const n = Math.max(...series.map(s=>s.data.length),1);
  const X=i=>pad+iw*(n<=1?0.5:i/(n-1)); const Y=v=>h-pad-ih*((v-yMin)/(yMax-yMin));
  x.fillStyle='#94a3b8'; x.font='11px sans-serif';
  for(let k=0;k<=4;k++){const v=yMin+(yMax-yMin)*k/4, yy=Y(v); x.fillText(v.toFixed(1),4,yy+4); x.strokeStyle='#1e293b'; x.beginPath(); x.moveTo(pad,yy); x.lineTo(w-pad,yy); x.stroke();}
  series.forEach(s=>{x.strokeStyle=s.color;x.lineWidth=2;x.beginPath();s.data.forEach((v,i)=>{const xx=X(i),yy=Y(Math.max(yMin,Math.min(yMax,v))); i?x.lineTo(xx,yy):x.moveTo(xx,yy)});x.stroke();});
}
function heat(frames){
  const c=document.getElementById('heatChart'), dpr=window.devicePixelRatio||1,w=c.clientWidth,h=c.clientHeight;c.width=w*dpr;c.height=h*dpr;const x=c.getContext('2d');x.scale(dpr,dpr);
  const rows=frames.length, cols=32, cw=w/cols, ch=h/Math.max(rows,1); let max=1; frames.forEach(f=>(f.heatmap||[]).forEach(v=>max=Math.max(max,v)));
  frames.forEach((f,r)=>(f.heatmap||[]).forEach((v,i)=>{const q=Math.sqrt(v/max);x.fillStyle='rgb('+Math.round(30+225*q)+','+Math.round(20+80*(1-q))+','+Math.round(60+120*(1-q))+')';x.fillRect(i*cw,r*ch,Math.ceil(cw),Math.ceil(ch));}));
}
function stageDot(st){return '<span class="stage '+st+'" title="'+st+'"></span>'}
fetch('/api/report').then(r=>r.json()).then(r=>{
  document.getElementById('meta').innerHTML = 'identity <code>'+r.identity.content_hash.slice(0,16)+'</code> param <code>'+r.identity.param_hash.slice(0,12)+'</code> commit <code>'+JSON.stringify(r.commit_order)+'</code>';
  const fr=r.frames||[];
  drawLine('snrChart',[{name:'PSNR',color:'#38bdf8',data:fr.map(f=>db(f.psnr_db_fixed,f.psnr_infinite))},{name:'SNR',color:'#a78bfa',data:fr.map(f=>db(f.snr_db_fixed,f.snr_infinite))}],20,120);
  drawLine('ssimChart',[{name:'SSIM',color:'#34d399',data:fr.map(f=>f.ssim_fixed/scale)}],0,1);
  drawLine('noiseChart',[{name:'noise',color:'#fb7185',data:fr.map(f=>f.noise_floor_std_fixed/scale)},{name:'bound',color:'#fbbf24',data:fr.map(f=>f.bound.rms_error_upper_fixed/scale)}],0,Math.max(10,...fr.map(f=>f.bound.rms_error_upper_fixed/scale)));
  heat(fr);
  const m=document.getElementById('matrix'); (r.blocks||[]).forEach(b=>{const states=Object.fromEntries((b.states||[]).map(s=>[s.stage,s.state]));const div=document.createElement('div');div.className='block';div.innerHTML='<b>块 '+b.block.index+'</b><br/>帧 '+b.block.first_frame+'-'+b.block.last_frame+'<br/>'+['decode','evaluate','bound','checkpoint'].map(s=>stageDot(states[s]||'pending')+s).join('<br/>');m.appendChild(div);});
  const tb=document.querySelector('#queueTable tbody');(r.queue||[]).forEach(q=>{const tr=document.createElement('tr');tr.innerHTML='<td>'+q.block_index+'</td><td>'+(q.reason||'')+'</td><td>'+q.enqueue_wait_ns+'</td><td>'+q.schedule_wait_ns+'</td><td>'+q.total_wait_ns+'</td><td>'+q.worker_id+'</td><td>'+q.queue_depth_at_submit+'</td><td>'+q.buffer_samples+'/'+q.max_buffer_samples+'</td>';tb.appendChild(tr);});
}).catch(e=>{document.getElementById('meta').textContent='report load failed: '+e});
</script>
</body></html>`
