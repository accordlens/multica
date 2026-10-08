import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { test } from 'node:test';

const digest = `sha256:${'a'.repeat(64)}`;
const env = { ...process.env, JWT_SECRET: 'config-test-only', MULTICA_IMAGE_TAG: 'must-not-be-appended',
  MULTICA_RELEASE_BACKEND_IMAGE: `example.invalid/fork-backend@${digest}`,
  MULTICA_RELEASE_WEB_IMAGE: `example.invalid/fork-web@${digest}`,
  MULTICA_RELEASE_DATABASE_IMAGE: `pgvector/pgvector@${digest}`,
  MULTICA_RELEASE_PLATFORM: 'linux/amd64' };
const args = ['compose', '--env-file', '.env.example', '-f', 'docker-compose.selfhost.yml',
  '-f', 'docker-compose.selfhost.release.yml', 'config', '--format', 'json'];

test('full release references survive Compose merge; deploy never builds/pulls a mutable tag', () => {
  const result = spawnSync('docker', args, { env, encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  const { services, volumes } = JSON.parse(result.stdout);
  for (const [service, variable] of [['backend', 'BACKEND'], ['frontend', 'WEB'], ['postgres', 'DATABASE']]) {
    assert.equal(services[service].image, env[`MULTICA_RELEASE_${variable}_IMAGE`]);
    assert.equal(services[service].platform, 'linux/amd64');
    assert.equal(services[service].pull_policy, 'never');
    assert.equal(services[service].build, undefined);
  }
  for (const service of ['backend', 'frontend']) assert.equal(services[service].ports[0].host_ip, '127.0.0.1');
  assert.ok(volumes.pgdata);
  assert.ok(volumes.backend_uploads);
});

test('missing release selections fail closed instead of using upstream/latest', () => {
  for (const variable of ['BACKEND', 'WEB', 'DATABASE']) {
    const missing = { ...env };
    delete missing[`MULTICA_RELEASE_${variable}_IMAGE`];
    const result = spawnSync('docker', args, { env: missing, encoding: 'utf8' });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /Select the .* from release.env/);
  }
});

test('offline release accepts local image IDs without attaching a tag', () => {
  const offline = { ...env, MULTICA_RELEASE_BACKEND_IMAGE: digest, MULTICA_RELEASE_WEB_IMAGE: digest,
    MULTICA_RELEASE_DATABASE_IMAGE: digest };
  const result = spawnSync('docker', args, { env: offline, encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  const config = JSON.parse(result.stdout);
  assert.equal(config.services.backend.image, digest);
});
