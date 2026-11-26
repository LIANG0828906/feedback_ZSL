package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"dialogueforge/dialogue"
)

type workbench struct {
	mu         sync.Mutex
	pipeline   *dialogue.Pipeline
	store      *dialogue.CheckpointStore
	result     *dialogue.BuildResult
	lastError  string
	lastCode   string
	samplePath string
}

type stateView struct {
	DialogueID    string                     `json:"dialogue_id"`
	SourceVersion string                     `json:"source_version"`
	TargetLoud    int64                      `json:"target_loudness"`
	GainLimit     int64                      `json:"gain_limit"`
	FrameLength   int64                      `json:"frame_length"`
	Hop           int64                      `json:"hop"`
	Progress      dialogue.Progress          `json:"progress"`
	Scenes        []sceneView                `json:"scenes"`
	Segments      []segmentView              `json:"segments"`
	Invalidation  *dialogue.InvalidationPlan `json:"invalidation,omitempty"`
	Budget        budgetView                 `json:"budget"`
	CommitOrder   []string                   `json:"commit_order"`
	Checkpoint    string                     `json:"checkpoint"`
	Error         string                     `json:"error,omitempty"`
	ErrorCode     string                     `json:"error_code,omitempty"`
	GeneratedAt   string                     `json:"generated_at"`
}

type sceneView struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	SegmentIDs []string `json:"segment_ids"`
}

type segmentView struct {
	ID                 string  `json:"id"`
	SceneID            string  `json:"scene_id"`
	Status             string  `json:"status"`
	SessionGain        int64   `json:"session_gain"`
	GainAdjustment     int64   `json:"gain_adjustment"`
	AppliedGain        int64   `json:"applied_gain"`
	BaseLoudness       int64   `json:"base_loudness"`
	CalibratedLoudness int64   `json:"calibrated_loudness"`
	Reached            bool    `json:"reached"`
	Envelope           []int64 `json:"envelope"`
	Calibrated         []int64 `json:"calibrated"`
	EnvelopeDigest     string  `json:"envelope_digest"`
	FrameStart         int64   `json:"frame_start_sample"`
	FrameLength        int64   `json:"frame_length"`
	Hop                int64   `json:"hop"`
}

type budgetView struct {
	Frames           int   `json:"frames"`
	Segments         int   `json:"segments"`
	Committed        int   `json:"committed_segments"`
	CacheBytes       int   `json:"cache_bytes"`
	CheckpointBytes  int64 `json:"checkpoint_bytes"`
	InvalidatedNodes int   `json:"invalidated_nodes"`
	ReusedNodes      int   `json:"reused_nodes"`
	FrameBudget      int   `json:"frame_budget"`
	CacheBudget      int   `json:"cache_budget_bytes"`
	CheckpointBudget int64 `json:"checkpoint_budget_bytes"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "local listen address")
	samplePath := flag.String("sample", "samples/dialogue-v3.json", "dialogue sample JSON")
	checkpoint := flag.String("checkpoint", "artifacts/workbench.checkpoint.json", "checkpoint file")
	seed := flag.Int64("seed", 42, "generator seed used when sample is absent")
	flag.Parse()

	project, err := loadOrGenerate(*samplePath, *seed)
	if err != nil {
		log.Fatal(err)
	}
	store := dialogue.NewCheckpointStore(*checkpoint)
	pipeline, err := dialogue.NewPipeline(project, store)
	if err != nil {
		log.Fatal(err)
	}
	wb := &workbench{pipeline: pipeline, store: store, samplePath: *samplePath}
	result, buildErr := pipeline.Build()
	wb.result = result
	if buildErr != nil {
		wb.recordError(buildErr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(pageHTML))
	})
	mux.HandleFunc("/api/state", wb.handleState)
	mux.HandleFunc("/api/edit", wb.handleEdit)
	mux.HandleFunc("/api/rebuild", wb.handleRebuild)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "algorithm": dialogue.AlgorithmVersion})
	})
	server := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("dialogue workbench listening on http://%s (sample=%s checkpoint=%s)", *addr, *samplePath, *checkpoint)
	log.Fatal(server.ListenAndServe())
}

func loadOrGenerate(path string, seed int64) (*dialogue.Project, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		loaded, err := dialogue.LoadProject(data)
		if err != nil {
			return nil, err
		}
		return loaded.Project, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	project, err := dialogue.GenerateProject(seed)
	if err != nil {
		return nil, err
	}
	return project, nil
}

func (w *workbench) handleState(rw http.ResponseWriter, _ *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	writeJSON(rw, http.StatusOK, w.viewLocked())
}

func (w *workbench) handleRebuild(rw http.ResponseWriter, _ *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	result, err := w.pipeline.Build()
	if result != nil {
		w.result = result
	}
	if err != nil {
		w.recordError(err)
	} else {
		w.lastError, w.lastCode = "", ""
	}
	writeJSON(rw, http.StatusOK, w.viewLocked())
}

func (w *workbench) handleEdit(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var edit dialogue.Edit
	if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.pipeline.ApplyEdit(edit); err != nil {
		w.recordError(err)
		writeJSON(rw, http.StatusUnprocessableEntity, w.viewLocked())
		return
	}
	result, err := w.pipeline.Build()
	if result != nil {
		w.result = result
	}
	if err != nil {
		w.recordError(err)
	} else {
		w.lastError, w.lastCode = "", ""
	}
	writeJSON(rw, http.StatusOK, w.viewLocked())
}

func (w *workbench) recordError(err error) {
	w.lastError = err.Error()
	w.lastCode = string(dialogue.CodeOf(err))
	if w.lastCode == "" {
		w.lastCode = "E_IO"
	}
}

func (w *workbench) viewLocked() stateView {
	project := w.pipeline.Project()
	plan := w.pipeline.LastPlan()
	state := w.pipeline.State().Snapshot()
	view := stateView{
		DialogueID: project.ID, SourceVersion: project.Source.SchemaVersion,
		TargetLoud: project.TargetLoudness, GainLimit: project.GainLimit,
		FrameLength: project.FrameLength, Hop: project.Hop,
		Progress: state.Progress, Invalidation: plan,
		Error: w.lastError, ErrorCode: w.lastCode, GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if w.store != nil {
		view.Checkpoint = w.store.Path()
	}
	for _, scene := range project.Scenes {
		sceneEntry := sceneView{ID: scene.ID, Name: scene.Name}
		for _, segment := range scene.Segments {
			sceneEntry.SegmentIDs = append(sceneEntry.SegmentIDs, segment.ID)
		}
		view.Scenes = append(view.Scenes, sceneEntry)
	}
	statusByID := map[string]string{}
	for _, stateSegment := range state.Segments {
		statusByID[stateSegment.SegmentID] = string(stateSegment.Status)
	}
	calibrationByID := map[string]*dialogue.SegmentCalibration{}
	envelopeByID := map[string]*dialogue.SegmentEnvelope{}
	if w.result != nil {
		for _, segment := range w.result.Segments {
			calibrationByID[segment.SegmentID] = segment.Calibration
			envelopeByID[segment.SegmentID] = segment.Envelope
			view.CommitOrder = append(view.CommitOrder, segment.SegmentID)
		}
		view.Budget = budgetView{
			Frames: w.result.Budget.Frames, Segments: w.result.Budget.Segments,
			Committed: w.result.Budget.CommittedSegments, CacheBytes: w.result.Budget.CacheBytes,
			CheckpointBytes:  w.result.Budget.CheckpointBytes,
			InvalidatedNodes: w.result.Budget.InvalidatedNodes, ReusedNodes: w.result.Budget.ReusedNodes,
			FrameBudget: 10000, CacheBudget: 8 << 20, CheckpointBudget: 4 << 20,
		}
	}
	for _, segment := range project.Segments() {
		envelope := envelopeByID[segment.ID]
		calibration := calibrationByID[segment.ID]
		entry := segmentView{
			ID: segment.ID, SceneID: segment.SceneID, Status: statusByID[segment.ID],
			SessionGain: segment.SessionGain, FrameStart: segment.StartSample,
			FrameLength: project.FrameLength, Hop: project.Hop,
		}
		if envelope != nil {
			entry.EnvelopeDigest = envelope.Digest()
			for _, frame := range envelope.Frames {
				entry.Envelope = append(entry.Envelope, frame.Envelope)
			}
		}
		if calibration != nil {
			entry.GainAdjustment = calibration.GainAdjustment
			entry.AppliedGain = calibration.AppliedGain
			entry.BaseLoudness = calibration.BaseLoudness
			entry.CalibratedLoudness = calibration.CalibratedLoudness
			entry.Reached = calibration.Reached
			for _, frame := range calibration.Frames {
				entry.Calibrated = append(entry.Calibrated, frame.Envelope)
			}
		}
		view.Segments = append(view.Segments, entry)
	}
	return view
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

const pageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>对白包络诊断台</title>
<style>
:root{--bg:#0b1018;--panel:#141c27;--line:#263244;--ink:#e8eef6;--muted:#8fa2b8;--accent:#54d6b4;--warn:#ffbd66;--bad:#ff6b7a;--blue:#6fa8ff}
*{box-sizing:border-box}body{margin:0;background:radial-gradient(circle at 20% 0%,#163041 0,#0b1018 42%);color:var(--ink);font:14px/1.45 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
header{display:flex;gap:18px;align-items:baseline;padding:18px 22px;border-bottom:1px solid var(--line)}
h1{font-size:18px;margin:0;letter-spacing:.02em}.meta{color:var(--muted);font-size:12px}
main{display:grid;grid-template-columns:minmax(0,1fr) 360px;gap:16px;padding:16px 22px 32px}
.panel{background:rgba(20,28,39,.9);border:1px solid var(--line);border-radius:10px;padding:14px}
.chart{min-height:390px}.toolbar{display:flex;gap:10px;flex-wrap:wrap;align-items:center;margin-bottom:10px}
button,select,input{background:#172334;color:var(--ink);border:1px solid var(--line);border-radius:6px;padding:6px 9px;font:inherit}
button{cursor:pointer}button:hover{border-color:var(--accent)}button.primary{background:#153c35;border-color:#2b8f79}
canvas{width:100%;height:340px;display:block}.legend{display:flex;gap:18px;color:var(--muted);font-size:12px}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:5px}
.segments{display:flex;gap:8px;flex-wrap:wrap;margin-top:10px}.seg{border:1px solid var(--line);border-radius:7px;padding:6px 8px;font-size:12px;cursor:pointer}
.seg.active{border-color:var(--accent);background:#123027}.seg.failed{border-color:var(--bad)}.seg.committed{box-shadow:inset 3px 0 0 var(--accent)}
.grid{display:grid;gap:12px}.kv{display:grid;grid-template-columns:120px 1fr;gap:5px 8px;font-size:12px}.kv span:nth-child(odd){color:var(--muted)}
.bar{height:6px;background:#0c131d;border-radius:4px;overflow:hidden;margin-top:4px}.bar>i{display:block;height:100%;background:var(--accent)}
.inv{max-height:190px;overflow:auto;font-size:11px;color:var(--muted);white-space:pre-wrap;word-break:break-all}
.status{display:inline-block;padding:2px 6px;border-radius:4px;background:#213146;font-size:11px}.status.committed{background:#163d34;color:#7af0c9}
.status.failed{background:#451f27;color:#ff9aa5}.err{border-color:var(--bad);color:#ffc4ca}
@media(max-width:900px){main{grid-template-columns:1fr}.chart{min-height:320px}canvas{height:270px}}
</style>
</head>
<body>
<header><h1>对白分段响度 / 包络诊断台</h1><div id="meta" class="meta">loading…</div></header>
<main>
  <section class="panel chart">
    <div class="toolbar">
      <select id="scene"></select>
      <button id="play" class="primary">播放</button>
      <input id="frame" type="range" min="0" max="0" value="0" style="flex:1;min-width:150px">
      <span id="frameLabel" class="meta">frame 0</span>
      <label><input id="showCal" type="checkbox" checked> 校准后</label>
      <button id="rebuild">重建</button>
      <button id="resetTarget">还原目标</button>
    </div>
    <div class="legend"><span><i class="dot" style="background:var(--blue)"></i>测得包络</span><span><i class="dot" style="background:var(--accent)"></i>校准包络</span><span>横轴：逻辑帧</span></div>
    <canvas id="canvas"></canvas>
    <div id="segments" class="segments"></div>
  </section>
  <aside class="grid">
    <section class="panel"><div class="kv" id="progress"></div><div id="error" class="meta"></div></section>
    <section class="panel"><h3 style="margin:0 0 8px;font-size:13px">预算占用</h3><div class="kv" id="budget"></div></section>
    <section class="panel"><h3 style="margin:0 0 8px;font-size:13px">片段状态</h3><div class="kv" id="segState"></div></section>
    <section class="panel"><h3 style="margin:0 0 8px;font-size:13px">失效清单</h3><div id="invalidation" class="inv"></div></section>
  </aside>
</main>
<script>
let state=null, activeScene=null, frame=0, playing=false, timer=null;
const $=id=>document.getElementById(id);
const fmt=n=>Number(n).toLocaleString();
async function loadState(){
  try{const r=await fetch('/api/state');state=await r.json();render();}
  catch(e){$('meta').textContent='state error: '+e;}
}
async function post(path,body){
  const r=await fetch(path,{method:'POST',headers:{'Content-Type':'application/json'},body:body?JSON.stringify(body):undefined});
  state=await r.json();render();
}
function currentSegments(){
  if(!state)return[];
  const s=state.scenes.find(x=>x.id===activeScene)||state.scenes[0];
  return s?s.segment_ids.map(id=>state.segments.find(x=>x.id===id)).filter(Boolean):[];
}
function totalFrames(){return currentSegments().reduce((n,s)=>n+s.envelope.length,0);}
function globalToLocal(g){
  let base=0;
  for(const s of currentSegments()){if(g<base+s.envelope.length)return{s,index:g-base};base+=s.envelope.length;}
  const list=currentSegments(); if(!list.length)return null; const s=list[list.length-1]; return {s,index:s.envelope.length-1};
}
function render(){
  if(!state)return;
  $('meta').textContent=state.dialogue_id+' · schema '+state.source_version+' · target '+state.target_loudness+' · frame '+state.frame_length+'/'+state.hop;
  if(!activeScene||!state.scenes.some(s=>s.id===activeScene))activeScene=state.scenes[0]?.id;
  const sceneSel=$('scene'); sceneSel.innerHTML='';
  for(const s of state.scenes){const o=document.createElement('option');o.value=s.id;o.textContent=s.name+' ('+s.id+')';sceneSel.appendChild(o);}
  if(activeScene)sceneSel.value=activeScene;
  const total=totalFrames(); $('frame').max=Math.max(0,total-1); if(frame>=total)frame=Math.max(0,total-1); $('frame').value=frame;
  $('frameLabel').textContent='frame '+(frame+1)+'/'+Math.max(1,total);
  const p=state.progress;
  $('progress').innerHTML='<span>提交进度</span><b>'+p.committed+'/'+p.total+'</b><span>目标响度</span><b>'+state.target_loudness+'</b><span>增益上界</span><b>±'+state.gain_limit+'</b><span>提交序列</span><b>'+(state.commit_order.join(' → ')||'—')+'</b>';
  $('error').className=state.error?'err meta':'meta'; $('error').textContent=state.error?(state.error_code+': '+state.error):'ready';
  const b=state.budget;
  $('budget').innerHTML='<span>帧</span><div>'+fmt(b.frames)+' / '+fmt(b.frame_budget)+'<div class="bar"><i style="width:'+Math.min(100,b.frames/b.frame_budget*100)+'%"></i></div></div><span>缓存</span><div>'+fmt(b.cache_bytes)+' B / '+fmt(b.cache_budget_bytes)+' B<div class="bar"><i style="width:'+Math.min(100,b.cache_bytes/b.cache_budget_bytes*100)+'%"></i></div></div><span>检查点</span><div>'+fmt(b.checkpoint_bytes)+' B / '+fmt(b.checkpoint_budget_bytes)+' B<div class="bar"><i style="width:'+Math.min(100,b.checkpoint_bytes/b.checkpoint_budget_bytes*100)+'%"></i></div></div><span>失效 / 复用节点</span><b>'+b.invalidated_nodes+' / '+b.reused_nodes+'</b>';
  $('segState').innerHTML=state.segments.map(s=>'<span>'+s.id+'</span><span><i class="status '+s.status+'">'+s.status+'</i> gain '+s.session_gain+' adj '+s.gain_adjustment+' loud '+s.base_loudness+'→'+s.calibrated_loudness+'</span>').join('');
  const inv=state.invalidation;
  $('invalidation').textContent=inv?('reason: '+inv.reason+'\nsegments: '+((inv.segment_ids||[]).join(', ')||'—')+'\norder:\n'+(inv.rebuild_order||[]).join('\n')+'\nreused: '+(inv.reused||[]).length):'—';
  const segs=currentSegments(); $('segments').innerHTML='';
  for(const s of segs){const d=document.createElement('div');d.className='seg '+s.status+(s.id===globalToLocal(frame)?.s.id?' active':'');d.textContent=s.id+' · '+s.status+' · '+s.envelope.length+'f';d.onclick=()=>{activeScene=s.scene_id;frame=0;render();};$('segments').appendChild(d);}
  draw();
}
function draw(){
  const cv=$('canvas'),dpr=window.devicePixelRatio||1,rect=cv.getBoundingClientRect();
  if(cv.width!==Math.round(rect.width*dpr)||cv.height!==Math.round(rect.height*dpr)){cv.width=Math.round(rect.width*dpr);cv.height=Math.round(rect.height*dpr);}
  const ctx=cv.getContext('2d');ctx.setTransform(dpr,0,0,dpr,0,0);const W=rect.width,H=rect.height;ctx.clearRect(0,0,W,H);
  ctx.strokeStyle='#263244';ctx.lineWidth=1;for(let i=0;i<=4;i++){const y=20+(H-40)*i/4;ctx.beginPath();ctx.moveTo(44,y);ctx.lineTo(W-12,y);ctx.stroke();ctx.fillStyle='#8fa2b8';ctx.font='10px ui-monospace';ctx.fillText(String(10000-i*2500),4,y+3);}
  const segs=currentSegments(),total=Math.max(1,totalFrames()),plotW=W-58,x0=44;
  const points=[];let base=0;
  for(const s of segs){for(let i=0;i<s.envelope.length;i++){points.push({x:x0+plotW*(base+i)/(total-1||1),a:s.envelope[i],c:s.calibrated[i],seg:s.id,idx:i,global:base+i});}base+=s.envelope.length;}
  function path(key,color,fill){if(!points.length)return;ctx.beginPath();ctx.strokeStyle=color;ctx.lineWidth=1.5;points.forEach((p,i)=>{const y=20+(H-40)*(1-Math.max(0,Math.min(12000,p[key]))/10000);i?ctx.lineTo(p.x,y):ctx.moveTo(p.x,y);});ctx.stroke();if(fill){ctx.lineTo(points[points.length-1].x,H-20);ctx.lineTo(points[0].x,H-20);ctx.closePath();ctx.fillStyle=color+'14';ctx.fill();}}
  path('a','#6fa8ff',true);if($('showCal').checked)path('c','#54d6b4',false);
  const loc=globalToLocal(frame);if(loc){const idx=segs.reduce((n,s)=>n+(s.id===loc.s.id?0:s.envelope.length),0)+loc.index;const x=x0+plotW*(idx/(total-1||1));ctx.strokeStyle='#ffbd66';ctx.beginPath();ctx.moveTo(x,16);ctx.lineTo(x,H-20);ctx.stroke();ctx.fillStyle='#ffbd66';ctx.font='11px ui-monospace';ctx.fillText(loc.s.id+' f'+loc.index,Math.min(W-120,x+4),16);}
  ctx.fillStyle='#8fa2b8';ctx.font='10px ui-monospace';ctx.fillText('frame',W-40,H-5);
}
function startPlay(){if(playing)return;playing=true;$('play').textContent='暂停';timer=setInterval(()=>{frame++;if(frame>=totalFrames())frame=0;render();},120);}
function stopPlay(){playing=false;$('play').textContent='播放';clearInterval(timer);}
$('scene').onchange=e=>{activeScene=e.target.value;frame=0;render();};
$('frame').oninput=e=>{frame=Number(e.target.value);render();};
$('showCal').onchange=draw;
$('play').onclick=()=>playing?stopPlay():startPlay();
$('rebuild').onclick=()=>post('/api/rebuild');
$('resetTarget').onclick=()=>post('/api/edit',{kind:'target-loudness',value:5600});
window.addEventListener('resize',draw);
loadState();setInterval(loadState,2500);
</script>
</body>
</html>`
