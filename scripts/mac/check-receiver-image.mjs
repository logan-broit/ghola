import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { setTimeout as delay } from 'node:timers/promises';

// Tests only a new, owned container/volume; never the deployed receiver or corpus.
const podman = (...args) => execFileSync('podman', args, { encoding: 'utf8', timeout: 30000 }).trim();
const owner = randomUUID(), name = `ghola-image-check-${owner}`;
let container, volume;
try {
  const image = process.argv[2];
  assert.ok(image, 'Usage: node scripts/mac/check-receiver-image.mjs IMAGE');
  assert.equal(podman('image', 'inspect', image, '--format', '{{.Config.User}}'), '10001:10001', 'receiver image must declare non-root UID/GID 10001');
  volume = podman('volume', 'create', '--label', `pi-ghola-check=${owner}`, name);
  container = podman('run', '-d', '--name', name, '--label', `pi-ghola-check=${owner}`,
    '-p', '127.0.0.1::7421', '-v', `${volume}:/var/lib/ghola/sessions`,
    '-e', 'GHOLA_ADDR=0.0.0.0:7421', '-e', 'GHOLA_LOOPBACK_ONLY=false',
    '-e', `AUTH_DEFAULT_USER=${owner}`, '-e', 'CHAPTERHOUSE_URL=http://127.0.0.1:1',
    '-e', 'CHAPTERHOUSE_REQUIRE_KEY=false', '-e', 'EMBEDDING_URL=', '-e', 'TRUTHSAYER_URL=',
    '-e', 'GHOLA_SESSION_IDLE_HOURS=0', '-e', 'GHOLA_SIETCH_RETENTION=0', image);
  const address = podman('port', container, '7421/tcp');
  assert.match(address, /^127\.0\.0\.1:\d+$/);
  const base = `http://${address}`;
  async function request(route, body) {
    const response = await fetch(base + route, { method: body ? 'POST' : 'GET',
      headers: { 'content-type': 'application/json' }, body: body ? JSON.stringify(body) : undefined,
      signal: AbortSignal.timeout(3000) });
    assert.equal(response.status, 200, `unexpected HTTP status for ${route}`);
    return response.json();
  }
  async function ready() {
    let last;
    for (let attempt = 0; attempt < 40; attempt++) {
      try {
        const health = await request('/health');
        assert.deepEqual(health.capabilities, { session_replay: 1, record_replay: 1 });
        return;
      } catch (error) { last = error; }
      await delay(250);
    }
    throw last;
  }
  await ready();
  assert.equal(podman('exec', container, 'id', '-u'), '10001');
  assert.equal(podman('exec', container, 'printenv', 'GHOLA_SESSIONS_DIR'), '/var/lib/ghola/sessions');
  const sessionId = randomUUID(), workspace = randomUUID();
  const start = { session_id: sessionId, user_id: owner, workspace_id: workspace, cwd: '/owned-image-check', agent_kind: 'fixture' };
  assert.equal((await request('/v1/session_start', start)).session.id, sessionId);
  assert.equal((await request('/v1/session_start', start)).session.id, sessionId);
  const text = 'nonrootcanary ' + '<>&中\n'.repeat(1000);
  const record = { session_id: sessionId, user_id: owner, source_key: 'image-check', event: { type: 'assistant', text } };
  const first = (await request('/v1/record', record)).event;
  assert.equal((await request('/v1/record', record)).event.id, first.id);
  await request('/v1/session_end', { session_id: sessionId, user_id: owner });
  podman('restart', container);
  await ready();
  assert.equal((await request('/v1/record', record)).event.id, first.id);
  const listed = (await request('/v1/list_sessions', { user_id: owner })).sessions;
  assert.equal(listed.length, 1); assert.equal(listed[0].event_count, 1); assert.ok(listed[0].ended_at);
  const recalled = await request('/v1/recall', { user_id: owner, workspace, session_id: sessionId,
    query_text: 'nonrootcanary', include_sietch: true, include_episode: false, include_semant: false, limit: 10 });
  assert.equal(recalled.hits.find(hit => hit.id === first.id)?.content, text);
  console.log(JSON.stringify({ image, uid: 10001, writableNamedVolume: true, replayAfterRestart: true,
    fullTextBytes: Buffer.byteLength(text), storedEvents: 1, closedEpisode: true, productionWrites: 0 }));
} catch (error) {
  console.error(error.stack); process.exitCode = 1;
} finally {
  try {
    if (container) podman('rm', '--force', container);
    if (volume) podman('volume', 'rm', volume);
  } catch (error) { console.error(`Owned fixture cleanup failed (${name}): ${error.message}`); process.exitCode = 1; }
}
