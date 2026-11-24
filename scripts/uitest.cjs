const fs = require("fs");
const path = require("path");
const { JSDOM } = require("jsdom");

const root = path.resolve(__dirname, "..");
const dashboard = JSON.parse(fs.readFileSync(process.argv[2], "utf8")).data;
const html = fs.readFileSync(path.join(root, "frontend/dist/index.html"), "utf8");
const js = fs.readFileSync(path.join(root, "frontend/dist/app.js"), "utf8");
const dom = new JSDOM(html, {
  url: "http://127.0.0.1:34115/",
  runScripts: "outside-only",
  pretendToBeVisual: true,
});
const { window } = dom;
const errors = [];
window.console.error = (...args) => errors.push(args.map((arg) => arg?.stack || String(arg)).join(" "));
window.console.warn = (...args) => errors.push(args.map((arg) => arg?.stack || String(arg)).join(" "));
window.addEventListener("error", (event) => errors.push(`window error: ${event.message}`));
window.addEventListener("unhandledrejection", (event) => errors.push(`rejection: ${event.reason}`));
window.devicePixelRatio = 1;
window.HTMLCanvasElement.prototype.getBoundingClientRect = () => ({
  width: 640,
  height: 370,
  left: 0,
  top: 0,
  right: 640,
  bottom: 370,
});
window.HTMLCanvasElement.prototype.getContext = () => {
  const gradient = { addColorStop() {} };
  const noop = () => {};
  return {
    setTransform: noop,
    clearRect: noop,
    fillRect: noop,
    strokeRect: noop,
    beginPath: noop,
    closePath: noop,
    moveTo: noop,
    lineTo: noop,
    stroke: noop,
    fill: noop,
    arc: noop,
    rect: noop,
    clip: noop,
    save: noop,
    restore: noop,
    fillText: noop,
    createLinearGradient: () => gradient,
  };
};
window.fetch = async () => ({
  status: 200,
  json: async () => ({ data: dashboard }),
});
window.eval(js);

setTimeout(() => {
  const required = ["windowMatrix", "chunkRows", "selectedTitle", "pathCards", "slotGrid"];
  for (const id of required) {
    if (!window.document.getElementById(id)) throw new Error(`missing DOM node ${id}`);
  }
  const windows = window.document.querySelectorAll(".window-card").length;
  const chunks = window.document.querySelectorAll("#chunkRows tr").length;
  const slots = window.document.querySelectorAll(".slot").length;
  if (windows === 0) {
    console.error("debug banner:", window.document.getElementById("globalBanner").textContent);
    console.error("debug toast:", window.document.getElementById("toast").textContent);
    console.error("debug errors:", errors);
  }
  if (windows !== dashboard.windows.length) {
    throw new Error(`window cards ${windows} != ${dashboard.windows.length}`);
  }
  if (chunks !== dashboard.selected.chunks.length) {
    throw new Error(`chunk rows ${chunks} != ${dashboard.selected.chunks.length}`);
  }
  if (slots !== dashboard.params.max_resident) {
    throw new Error(`slots ${slots} != ${dashboard.params.max_resident}`);
  }
  if (window.document.getElementById("kpiCommitted").textContent !==
      `${dashboard.progress.committed}/${dashboard.progress.total}`) {
    throw new Error("committed KPI was not rendered");
  }
  if (errors.length) {
    throw new Error(`console errors: ${errors.join(" | ")}`);
  }
  console.log(JSON.stringify({
    windows,
    chunks,
    slots,
    paths: dashboard.verification?.three_paths?.paths?.length || 0,
    rendered: true,
  }));
}, 500);
