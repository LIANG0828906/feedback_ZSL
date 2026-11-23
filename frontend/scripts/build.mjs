import { access, copyFile, mkdir, readdir } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const src = join(root, "src");
const dist = join(root, "dist");
const required = ["index.html", "styles.css", "app.js"];

async function validateDist() {
  for (const name of required) {
    await access(join(dist, name));
  }
  const entries = await readdir(dist);
  if (!entries.includes("index.html")) {
    throw new Error("frontend/dist/index.html is required for Wails asset embedding");
  }
  console.log(`frontend assets ready: ${entries.length} files in ${dist}`);
}

async function copySource() {
  try {
    await access(src);
  } catch {
    return false;
  }
  await mkdir(dist, { recursive: true });
  for (const name of await readdir(src)) {
    await copyFile(join(src, name), join(dist, name));
  }
  return true;
}

if (await copySource()) {
  console.log("copied static frontend sources to dist");
}
await validateDist();
