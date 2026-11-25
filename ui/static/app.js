const $ = (id) => document.getElementById(id);
let manifests = [];

async function loadSamples() {
  const r = await fetch('/api/samples').then(r => r.json());
  manifests = r.samples || [];
  $('image').innerHTML = manifests.map(m => `<option value="${m.image_id}">${m.image_id} (${m.source.w}×${m.source.h})</option>`).join('');
  if (manifests.length) selectImage(manifests[0].image_id);
}

function imageURL(name) {
  const id = $('image').value;
  return `/api/image?image_id=${encodeURIComponent(id)}&name=${name}&t=${Date.now()}`;
}

function selectImage(id) {
  $('image').value = id;
  $('source').src = imageURL('source');
  $('reference').src = imageURL('reference');
  $('corrected').src = imageURL('corrected');
  $('mask').src = imageURL('mask');
  const m = manifests.find(x => x.image_id === id);
  if (m) {
    $('tol').value = m.declared.segment.color_tolerance;
    $('grad').value = m.declared.segment.gradient_threshold;
    $('tolV').textContent = $('tol').value;
    $('gradV').textContent = $('grad').value;
  }
  refreshState();
}

async function run() {
  $('output').textContent = '运行中...';
  const image_id = $('image').value;
  const r = await fetch('/api/run', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({image_id})}).then(r=>r.json());
  $('output').textContent = JSON.stringify(summarizeRun(r), null, 2);
  $('corrected').src = imageURL('corrected');
  $('mask').src = imageURL('mask');
  refreshState();
}

function summarizeRun(r) {
  if (!r.report) return r;
  return {
    ok: r.ok,
    error: r.error,
    commits: r.report.committed_keys?.length,
    interrupted: r.report.interrupted,
    snapshot_reset: r.report.snapshot_reset,
    waits: r.report.waits?.length,
    stats: r.report.stats,
    validation_error: r.report.validation_error
  };
}

async function refreshState() {
  const id = $('image').value;
  if (!id) return;
  const r = await fetch(`/api/state?image_id=${encodeURIComponent(id)}`).then(r=>r.json());
  const stages = ['mapping','grow','clean','stats'];
  const by = {};
  (r.states||[]).forEach(s => by[`${s.tile_index}:${s.stage}`] = s);
  const count = (r.commit_count || 0) / 4;
  let html = '<table><thead><tr><th>tile</th>' + stages.map(s=>`<th>${s}</th>`).join('') + '</tr></thead><tbody>';
  const tiles = Array.from({length: Math.max(1, count)}, (_,i)=>i);
  const maxTile = Math.max(0, ...(r.states||[]).map(s=>s.tile_index));
  for (let i=0;i<=maxTile;i++) {
    html += `<tr><td>${i}</td>` + stages.map(stage => {
      const s = by[`${i}:${stage}`];
      const cls = s ? s.state : 'pending';
      const title = s?.error_message ? `${s.error_code||''} ${s.error_message}` : cls;
      return `<td><button class="cell ${cls}" title="${title}" data-tile="${i}" data-stage="${stage}">${cls}</button></td>`;
    }).join('') + '</tr>';
  }
  html += '</tbody></table>';
  $('matrix').innerHTML = html;
  document.querySelectorAll('button.cell.failed').forEach(btn => btn.onclick = () => retry(+btn.dataset.tile, btn.dataset.stage));
}

async function retry(tile, stage) {
  const image_id = $('image').value;
  await fetch('/api/retry', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({image_id,tile,stage})});
  await run();
}

let previewTimer;
async function preview() {
  const id = $('image').value;
  const tol = $('tol').value, grad = $('grad').value;
  $('tolV').textContent = tol; $('gradV').textContent = grad;
  clearTimeout(previewTimer);
  previewTimer = setTimeout(async () => {
    const r = await fetch(`/api/preview?image_id=${id}&color_tolerance=${tol}&gradient_threshold=${grad}`).then(r=>r.json());
    if (!r.ok) { $('preview').textContent = r.error; return; }
    const p = r.preview;
    $('preview').textContent = JSON.stringify({
      downsample_step: p.downsample_step,
      resolution_scale: p.resolution_scale,
      iteration_budget: p.iteration_budget,
      iterations_used: p.iterations_used,
      delay_ms: p.delay_ms,
      within_delay_limit: p.within_delay_limit,
      region_count: p.stats?.length,
      divergence: r.divergence
    }, null, 2);
  }, 60);
}

async function compare() {
  const id = $('image').value;
  $('output').textContent = '正在执行 full / breakpoint-resume 比对...';
  const r = await fetch(`/api/compare?image_id=${id}`).then(r=>r.json());
  $('output').textContent = JSON.stringify(r, null, 2);
}

async function verify() {
  const id = $('image').value;
  const r = await fetch(`/api/verify?image_id=${id}`).then(r=>r.json());
  $('output').textContent = JSON.stringify(r, null, 2);
}

async function control(action) {
  await fetch('/api/control', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({action})});
}

$('run').onclick = run;
$('compare').onclick = compare;
$('verify').onclick = verify;
$('refresh').onclick = refreshState;
$('pause').onclick = () => control('pause');
$('resume').onclick = () => control('resume');
$('cancel').onclick = () => control('cancel');
$('image').onchange = e => selectImage(e.target.value);
$('tol').oninput = preview;
$('grad').oninput = preview;
setInterval(refreshState, 1500);
loadSamples();
