// Throwaway Phase 0 test server. Node >= 20, zero dependencies.
//
// Its job is to be the *independent witness* for the spike: the phone can lie
// about what it did (or be suspended and never report back), so byte-arrival
// timestamps recorded here are the source of truth for the background-upload
// question.
//
//   node server.mjs [--port 8787] [--no-mdns]

import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import os from 'node:os';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const UPLOAD_DIR = path.join(HERE, 'uploads');
const SERVICE_TYPE = '_photobackup._tcp';
const SERVICE_NAME = 'photod-spike';

const args = process.argv.slice(2);
const PORT = Number(argOf('--port') ?? 8787);
const MDNS = !args.includes('--no-mdns');

function argOf(flag) {
  const i = args.indexOf(flag);
  return i === -1 ? undefined : args[i + 1];
}

fs.mkdirSync(UPLOAD_DIR, { recursive: true });

// ---------------------------------------------------------------- timeline

const START = Date.now();
/** @type {{seq:number, at:string, ms:number, event:string, detail:object}[]} */
const timeline = [];
let seq = 0;

function record(event, detail = {}) {
  const now = Date.now();
  const entry = { seq: ++seq, at: new Date(now).toISOString(), ms: now - START, event, detail };
  timeline.push(entry);
  if (timeline.length > 5000) timeline.splice(0, timeline.length - 5000);
  const pretty = Object.entries(detail)
    .map(([k, v]) => `${k}=${typeof v === 'object' ? JSON.stringify(v) : v}`)
    .join(' ');
  console.log(`[${new Date(now).toISOString().slice(11, 23)}] ${event.padEnd(18)} ${pretty}`);
  return entry;
}

// ------------------------------------------------------------------ routes

function json(res, status, body) {
  const payload = JSON.stringify(body);
  res.writeHead(status, {
    'content-type': 'application/json',
    'content-length': Buffer.byteLength(payload),
  });
  res.end(payload);
}

function handleUpload(req, res, url) {
  const testId = req.headers['x-test-id'] ?? `anon-${Date.now()}`;
  const clientMd5 = req.headers['x-client-md5'] ?? null;
  const filename = req.headers['x-filename'] ?? 'unnamed.bin';
  const declaredSize = Number(req.headers['x-declared-size'] ?? 0) || null;
  // Bytes/sec. Slow enough that we have time to background the app mid-flight.
  const throttle = Number(url.searchParams.get('throttle') ?? 0) || 0;

  const safeId = String(testId).replace(/[^a-zA-Z0-9._-]/g, '_');
  const dest = path.join(UPLOAD_DIR, `${safeId}.bin`);
  const out = fs.createWriteStream(dest);
  const hash = crypto.createHash('md5');
  const sha = crypto.createHash('sha256');

  let received = 0;
  let firstByteAt = null;
  let lastTickBytes = 0;

  record('upload-start', {
    testId,
    filename,
    throttle: throttle || 'none',
    declaredSize: declaredSize ?? '?',
    clientMd5: clientMd5 ?? '-',
    contentLength: req.headers['content-length'] ?? '-',
  });

  // A heartbeat rather than per-chunk logging: it makes a stall during app
  // suspension visible as a gap in the timeline instead of just an absence.
  const tick = setInterval(() => {
    const delta = received - lastTickBytes;
    lastTickBytes = received;
    record('upload-tick', {
      testId,
      received,
      deltaBytes: delta,
      pct: declaredSize ? ((received / declaredSize) * 100).toFixed(1) : '?',
    });
  }, 1000);

  req.on('data', (chunk) => {
    if (firstByteAt === null) {
      firstByteAt = Date.now();
      record('upload-first-byte', { testId });
    }
    received += chunk.length;
    hash.update(chunk);
    sha.update(chunk);
    out.write(chunk);

    if (throttle > 0) {
      req.pause();
      setTimeout(() => req.resume(), (chunk.length / throttle) * 1000);
    }
  });

  req.on('aborted', () => {
    clearInterval(tick);
    out.end();
    record('upload-aborted', { testId, received, afterMs: Date.now() - (firstByteAt ?? START) });
  });

  req.on('error', (err) => {
    clearInterval(tick);
    out.end();
    record('upload-error', { testId, received, error: err.message });
  });

  req.on('end', () => {
    clearInterval(tick);
    out.end(() => {
      const serverMd5 = hash.digest('hex');
      const serverSha256 = sha.digest('hex');
      const durationMs = Date.now() - (firstByteAt ?? START);
      const md5Match = clientMd5 ? clientMd5.toLowerCase() === serverMd5 : null;
      record('upload-complete', {
        testId,
        received,
        durationMs,
        mbPerSec: (received / 1024 / 1024 / (durationMs / 1000)).toFixed(2),
        serverMd5,
        md5Match: md5Match === null ? 'no-client-md5' : md5Match,
      });
      json(res, 200, {
        ok: true,
        testId,
        received,
        durationMs,
        serverMd5,
        serverSha256,
        clientMd5,
        md5Match,
        savedAs: dest,
      });
    });
  });
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://${req.headers.host ?? 'localhost'}`);
  res.setHeader('access-control-allow-origin', '*');

  if (url.pathname === '/health') {
    record('health', { ua: req.headers['user-agent'] ?? '-', from: req.socket.remoteAddress });
    return json(res, 200, {
      ok: true,
      name: SERVICE_NAME,
      uptimeMs: Date.now() - START,
      serverTime: new Date().toISOString(),
    });
  }

  if (url.pathname === '/upload' && (req.method === 'POST' || req.method === 'PUT')) {
    return handleUpload(req, res, url);
  }

  if (url.pathname === '/timeline' && req.method === 'GET') {
    const since = Number(url.searchParams.get('since') ?? 0);
    return json(res, 200, { start: START, events: timeline.filter((e) => e.seq > since) });
  }

  if (url.pathname === '/timeline' && req.method === 'DELETE') {
    timeline.length = 0;
    seq = 0;
    record('timeline-reset');
    return json(res, 200, { ok: true });
  }

  // Lets the phone push its own observations into the same ordered timeline as
  // the server's, so client and server events can be read against one clock.
  if (url.pathname === '/note' && req.method === 'POST') {
    let body = '';
    req.on('data', (c) => (body += c));
    req.on('end', () => {
      let detail = { raw: body.slice(0, 500) };
      try {
        detail = JSON.parse(body);
      } catch {}
      record('client-note', detail);
      json(res, 200, { ok: true });
    });
    return;
  }

  json(res, 404, { ok: false, error: 'not found', path: url.pathname });
});

// -------------------------------------------------------------------- mdns

let mdnsProc = null;
function startMdns() {
  // macOS ships dns-sd; using it avoids pulling an mDNS library into a
  // throwaway server, and it advertises the exact service type v1 will use.
  mdnsProc = spawn('dns-sd', [
    '-R', SERVICE_NAME, SERVICE_TYPE, 'local.', String(PORT),
    'path=/', 'ver=1', 'srv=photobackup-spike',
  ]);
  mdnsProc.stdout.on('data', (d) => {
    const line = String(d).trim();
    if (line) console.log(`[mdns] ${line}`);
  });
  mdnsProc.stderr.on('data', (d) => console.log(`[mdns:err] ${String(d).trim()}`));
  mdnsProc.on('exit', (code) => console.log(`[mdns] dns-sd exited code=${code}`));
}

function lanAddresses() {
  return Object.entries(os.networkInterfaces())
    .flatMap(([name, addrs]) => (addrs ?? []).map((a) => ({ name, ...a })))
    .filter((a) => a.family === 'IPv4' && !a.internal)
    .map((a) => `${a.address} (${a.name})`);
}

server.listen(PORT, '0.0.0.0', () => {
  console.log('phase0 test server');
  console.log(`  listening on 0.0.0.0:${PORT}`);
  for (const a of lanAddresses()) console.log(`  reachable at http://${a.split(' ')[0]}:${PORT}`);
  console.log(`  uploads -> ${UPLOAD_DIR}`);
  if (MDNS) startMdns();
  else console.log('  mdns advertising disabled (--no-mdns)');
  console.log('');
});

for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, () => {
    mdnsProc?.kill();
    server.close(() => process.exit(0));
    setTimeout(() => process.exit(0), 500);
  });
}
