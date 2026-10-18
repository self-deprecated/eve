// Synthesizes the soundtrack (23 s, 44.1 kHz stereo WAV) in C major to match the scene timings.
import { writeFileSync } from "node:fs";

const SR = 44100, DUR = 23.0, N = Math.round(SR * DUR);
const L = new Float32Array(N), R = new Float32Array(N);
const midi = m => 440 * Math.pow(2, (m - 69) / 12);
const BEAT = 60 / 96;

function add(start, len, fn, pan = 0, gain = 1) {
  const s0 = Math.round(start * SR), n = Math.round(len * SR);
  const gl = gain * Math.cos((pan + 1) * Math.PI / 4), gr = gain * Math.sin((pan + 1) * Math.PI / 4);
  for (let i = 0; i < n && s0 + i < N; i++) {
    if (s0 + i < 0) continue;
    const v = fn(i / SR);
    L[s0 + i] += v * gl; R[s0 + i] += v * gr;
  }
}

// Soft pad: detuned triangles through a one-pole lowpass, slow attack and release.
function pad(start, len, notes, gain = 0.06, cutoff = 1200) {
  for (const m of notes) for (const [det, pan] of [[-0.08, -0.6], [0.08, 0.6]]) {
    const f = midi(m + det); let y = 0; const a = 1 - Math.exp(-2 * Math.PI * cutoff / SR);
    add(start, len + 1.2, t => {
      const ph = (t * f) % 1, tri = 4 * Math.abs(ph - 0.5) - 1;
      y += a * (tri - y);
      const env = Math.min(1, t / 0.6) * (t > len ? Math.exp(-(t - len) * 3) : 1);
      return y * env;
    }, pan, gain);
  }
}
const pluck = (start, m, gain = 0.12, pan = 0, decay = 5) =>
  add(start, 1.2, t => (Math.sin(2 * Math.PI * midi(m) * t) + 0.3 * Math.sin(4 * Math.PI * midi(m) * t)) *
    Math.exp(-t * decay) * Math.min(1, t * 400), pan, gain);
const bell = (start, m, gain = 0.1, pan = 0) =>
  add(start, 2.5, t => (Math.sin(2 * Math.PI * midi(m) * t) + 0.4 * Math.sin(2 * Math.PI * midi(m) * 2.76 * t) * Math.exp(-t * 4)) *
    Math.exp(-t * 1.6) * Math.min(1, t * 300), pan, gain);
const kick = (start, gain = 0.35) =>
  add(start, 0.4, t => Math.sin(2 * Math.PI * (45 * t + 60 * (1 - Math.exp(-t * 30)) / 30)) * Math.exp(-t * 9), 0, gain);
let seed = 7; const rnd = () => ((seed = (seed * 16807) % 2147483647) / 2147483647) * 2 - 1;
const tick = (start, gain = 0.05, pan = 0.3) => { let y = 0; add(start, 0.05, t => { const x = rnd(); const v = x - y; y = x; return v * Math.exp(-t * 120); }, pan, gain); };

// Hook (0–4.3): a nervous loop on A minor, ticking like a poll.
for (let t = 0; t < 4.2; t += BEAT / 2) {
  pluck(t, [57, 60, 64, 60][Math.round(t / (BEAT / 2)) % 4], 0.07, -0.2, 7);
  tick(t + BEAT / 4, 0.04);
}
pad(0, 4.2, [45, 52], 0.05, 600);

// Reveal (4.6): the loop stops; a bright chord opens.
pad(4.6, 2.8, [48, 55, 64, 71], 0.055, 1500);
bell(4.6, 72, 0.09, -0.2); bell(4.75, 79, 0.06, 0.3);
kick(4.6, 0.3);

// Ship → fail → human (7.7–19.9): C – Am – F – G with a gentle arpeggio.
const prog = [[48, 55, 64, 67], [45, 52, 60, 64], [41, 48, 57, 60], [43, 50, 59, 62]];
const bar = BEAT * 4;
for (let b = 0, t = 7.7; t < 19.9; b++, t += bar) {
  const ch = prog[b % 4];
  pad(t, Math.min(bar, 19.9 - t), ch, 0.045, 1300);
  for (let k = 0; k < 8 && t + k * BEAT / 2 < 19.9; k++)
    pluck(t + k * BEAT / 2, ch[1 + (k % 3)] + 12, 0.05, k % 2 ? 0.4 : -0.4, 6);
  for (let k = 0; k < 4 && t + k * BEAT < 19.9; k++) { kick(t + k * BEAT, 0.22); tick(t + k * BEAT + BEAT / 2, 0.025, -0.3); }
}
// Success chime when SUCCEEDED appears (ship starts 7.7, result at +4.2).
[76, 79, 84].forEach((m, i) => bell(11.9 + i * 0.09, m, 0.08, -0.3 + i * 0.3));
// A low note under "exit 65" and a soft two-note ask under "exit 78".
pluck(13.6, 40, 0.15, 0, 3);
bell(17.4, 76, 0.06, -0.2); bell(17.7, 74, 0.06, 0.2);

// Outro (20.2–23): resolve on C and fade.
pad(20.2, 2.0, [36, 48, 55, 64, 72], 0.05, 1600);
bell(20.2, 84, 0.07, 0); kick(20.2, 0.3);

// Light reverb: four combs per channel.
for (const ch of [L, R]) {
  const dry = Float32Array.from(ch);
  for (const [d, g] of [[1557, 0.5], [1617, 0.48], [1491, 0.46], [1422, 0.44]]) {
    const dd = d + (ch === R ? 23 : 0), buf = new Float32Array(N);
    for (let i = dd; i < N; i++) buf[i] = dry[i - dd] + g * buf[i - dd];
    for (let i = 0; i < N; i++) ch[i] += buf[i] * 0.07;
  }
}

// Fade the last second, normalize to -1 dBFS, write 16-bit PCM.
let peak = 0;
for (let i = 0; i < N; i++) {
  const f = Math.min(1, (N - i) / SR);
  L[i] *= f; R[i] *= f; peak = Math.max(peak, Math.abs(L[i]), Math.abs(R[i]));
}
const k = 0.89 / peak, out = Buffer.alloc(44 + N * 4);
out.write("RIFF", 0); out.writeUInt32LE(36 + N * 4, 4); out.write("WAVE", 8); out.write("fmt ", 12);
out.writeUInt32LE(16, 16); out.writeUInt16LE(1, 20); out.writeUInt16LE(2, 22); out.writeUInt32LE(SR, 24);
out.writeUInt32LE(SR * 4, 28); out.writeUInt16LE(4, 32); out.writeUInt16LE(16, 34); out.write("data", 36); out.writeUInt32LE(N * 4, 40);
for (let i = 0; i < N; i++) { out.writeInt16LE(Math.round(L[i] * k * 32767), 44 + i * 4); out.writeInt16LE(Math.round(R[i] * k * 32767), 46 + i * 4); }
writeFileSync(process.argv[2] ?? "music.wav", out);
console.log("peak", peak.toFixed(2));
