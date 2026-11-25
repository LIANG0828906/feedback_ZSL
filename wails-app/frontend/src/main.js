import './style.css';
import './app.css';

const app = document.querySelector('#app');
app.innerHTML = `
  <main class="prosody-shell">
    <header class="prosody-bar">
      <strong>韵律包络对齐与时长拨配诊断台</strong>
      <span id="bridgeStatus">正在启动本地 Go 诊断服务…</span>
    </header>
    <iframe id="diagnosticFrame" title="Prosody diagnostic interface"></iframe>
  </main>
`;

async function connect() {
  try {
    const url = await window.go.main.App.DiagnosticURL();
    document.getElementById('bridgeStatus').textContent = url;
    document.getElementById('diagnosticFrame').src = url;
  } catch (err) {
    document.getElementById('bridgeStatus').textContent =
      'Wails bridge unavailable; run `go run ./cmd/diag` in the repository root.';
    console.error(err);
  }
}

connect();
