// Renders index.html frame by frame through Chrome's DevTools protocol (no dependencies).
// Usage: node render.mjs <html> <framesDir> [fps] [onlySeconds,comma,separated]
//
// The README video, from this directory (needs chromium and ffmpeg):
//   mkdir -p /tmp/eve-frames && node render.mjs index.html /tmp/eve-frames && node music.mjs /tmp/eve-music.wav
//   ffmpeg -framerate 30 -i /tmp/eve-frames/f%04d.png -i /tmp/eve-music.wav -c:v libx264 -preset slow -crf 20 \
//     -pix_fmt yuv420p -c:a aac -b:a 160k -shortest -movflags +faststart ../eve.mp4
//   ffmpeg -i ../eve.mp4 -vf "fps=12,scale=800:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=64[p];[b][p]paletteuse=dither=none:diff_mode=rectangle" ../eve.gif
import { spawn } from "node:child_process";
import { writeFileSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const [html, outDir, fpsArg = "30", only] = process.argv.slice(2);
const fps = Number(fpsArg);
const port = 9333;
const profile = mkdtempSync(join(tmpdir(), "eve-chrome-"));
const chrome = spawn("chromium", [
  "--headless=new", `--remote-debugging-port=${port}`, `--user-data-dir=${profile}`,
  "--window-size=1920,1080", "--hide-scrollbars", "--force-device-scale-factor=1", "about:blank",
], { stdio: "ignore" });

let targets;
for (let i = 0; i < 50 && !targets; i++) {
  await new Promise(r => setTimeout(r, 200));
  targets = await fetch(`http://127.0.0.1:${port}/json`).then(r => r.json()).catch(() => null);
}
const page = targets.find(t => t.type === "page");
const ws = new WebSocket(page.webSocketDebuggerUrl);
await new Promise(r => ws.addEventListener("open", r));
let id = 0;
const pending = new Map();
ws.addEventListener("message", e => {
  const m = JSON.parse(e.data);
  if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
});
const send = (method, params = {}) => new Promise((res, rej) => {
  const n = ++id;
  pending.set(n, m => (m.error ? rej(new Error(`${method}: ${m.error.message}`)) : res(m.result)));
  ws.send(JSON.stringify({ id: n, method, params }));
});
const evaluate = async expr => {
  const r = await send("Runtime.evaluate", { expression: expr, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description ?? r.exceptionDetails.text);
  return r.result.value;
};

await send("Emulation.setDeviceMetricsOverride", { width: 1920, height: 1080, deviceScaleFactor: 1, mobile: false });
await send("Page.enable");
await send("Page.navigate", { url: "file://" + resolve(html) });
for (let i = 0; i < 50 && !(await evaluate("typeof window.ready !== 'undefined'").catch(() => false)); i++)
  await new Promise(r => setTimeout(r, 100));
await evaluate("window.ready");
const duration = await evaluate("window.DURATION");

const times = only ? only.split(",").map(Number) : Array.from({ length: Math.round(duration * fps) }, (_, i) => i / fps);
for (const [i, t] of times.entries()) {
  await evaluate(`render(${t})`);
  const { data } = await send("Page.captureScreenshot", { format: "png" });
  const name = only ? `still-${t.toFixed(2)}.png` : `f${String(i).padStart(4, "0")}.png`;
  writeFileSync(join(outDir, name), Buffer.from(data, "base64"));
}
console.log(`wrote ${times.length} frames`);
ws.close();
chrome.kill();
