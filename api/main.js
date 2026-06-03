'use strict';

const http = require('http');

const PORT = process.env.PORT || '3000';
const COMPUTE_SERVICE_URL = process.env.COMPUTE_SERVICE_URL || 'http://compute-service.demo:8080';

// --- structured JSON logger (mirrors compute service slog format) ----------

function log(level, msg, fields = {}) {
  const line = { time: new Date().toISOString(), level, msg, ...fields };
  process.stdout.write(JSON.stringify(line) + '\n');
}

// --- helpers ---------------------------------------------------------------

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (chunk) => chunks.push(chunk));
    req.on('end', () => resolve(Buffer.concat(chunks).toString()));
    req.on('error', reject);
  });
}

function send(res, status, body) {
  const payload = JSON.stringify(body);
  res.writeHead(status, {
    'Content-Type': 'application/json',
    'Content-Length': Buffer.byteLength(payload),
  });
  res.end(payload);
}

// --- route handlers --------------------------------------------------------

async function handleHealth(req, res) {
  send(res, 200, { status: 'ok' });
}

async function handleCompute(req, res) {
  if (req.method !== 'POST') {
    send(res, 405, { error: 'method not allowed' });
    return 405;
  }

  let body;
  try {
    body = JSON.parse(await readBody(req));
  } catch {
    send(res, 400, { error: 'invalid JSON body' });
    return 400;
  }

  if (typeof body.n !== 'number') {
    send(res, 400, { error: 'body must contain numeric field "n"' });
    return 400;
  }

  // forward to compute service
  let result;
  try {
    result = await fetch(`${COMPUTE_SERVICE_URL}/compute`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ n: body.n }),
    });
  } catch (err) {
    log('error', 'compute_upstream_error', { error: err.message });
    send(res, 502, { error: 'compute service unreachable' });
    return 502;
  }

  if (!result.ok) {
    const text = await result.text();
    log('error', 'compute_upstream_error', { status: result.status, body: text });
    send(res, 502, { error: 'compute service error', upstream_status: result.status });
    return 502;
  }

  const data = await result.json();
  send(res, 200, data);
  return 200;
}

// --- router + logging middleware -------------------------------------------

async function router(req, res) {
  const start = Date.now();
  const { method, url } = req;

  let status;
  try {
    if (url === '/api/health' || url === '/api/health/') {
      await handleHealth(req, res);
      status = 200;
    } else if (url === '/api/compute' || url === '/api/compute/') {
      status = await handleCompute(req, res);
    } else {
      send(res, 404, { error: 'not found' });
      status = 404;
    }
  } catch (err) {
    log('error', 'unhandled_error', { error: err.message, stack: err.stack });
    send(res, 500, { error: 'internal server error' });
    status = 500;
  }

  log('info', 'request', {
    method,
    path: url,
    status,
    duration_ms: Date.now() - start,
  });
}

// --- start -----------------------------------------------------------------

const server = http.createServer(router);

server.listen(PORT, () => {
  log('info', 'starting', {
    port: PORT,
    compute_service_url: COMPUTE_SERVICE_URL,
  });
});
