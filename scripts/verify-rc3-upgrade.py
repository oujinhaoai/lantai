#!/usr/bin/env python3
"""Verify an rc.2 -> rc.3 image-only upgrade on disposable CI resources.

Reuses released images, the repository Compose template and the existing first
start test's image/log helpers. No NAS connection and no volume deletion.
Only the small evidence directory is uploaded; temporary credentials stay local.
"""
import argparse
import base64
import hashlib
import hmac
import http.client
import importlib.util
import json
import os
from pathlib import Path
import secrets
import socket
import sqlite3
import ssl
import struct
import subprocess
import tarfile
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / 'upgrade-evidence'
ENV = {k: v for k, v in os.environ.items() if not k.startswith(('LANTAI_', 'COMPOSE_'))}
BASE = '676de7aa5777e6e71e10d68ef98331b87776b8b8'
TARGET = '9bf4b56b08deb4e5ea6ba4f294629e51df1bc9f9'
RESULT = {'schema': 'lantai.synthetic-image-upgrade/v1', 'status': 'running',
          'old_commit': BASE, 'new_commit': TARGET, 'nas_access': False,
          'production_credentials': False, 'checks': [], 'volume_deletion': False}


def run(args, timeout=180):
    p = subprocess.run(args, capture_output=True, text=True, env=ENV, timeout=timeout)
    if p.returncode:
        code = None
        try:
            e = json.loads(p.stderr)
            code = e.get('code') or e.get('error', {}).get('code')
        except (ValueError, AttributeError):
            pass
        RESULT['failed_command'] = {'program': Path(args[0]).name, 'exit_code': p.returncode, 'error_code': code}
        raise RuntimeError('Command failed in phase ' + RESULT.get('phase', 'initialization'))
    return p.stdout.strip()


def check(name, condition=True, **details):
    if not condition:
        raise AssertionError(name)
    RESULT['checks'].append({'name': name, 'passed': True, **details})
    print('PASS: ' + name, flush=True)


def save():
    OUT.mkdir(exist_ok=True)
    (OUT / 'result.json').write_text(json.dumps(RESULT, indent=2) + '\n')
    rows = ['# Isolated rc.2 to rc.3 upgrade', '', 'Status: ' + RESULT['status'], '',
            'This is a synthetic GitHub-hosted Linux Docker test, not a NAS run.', '']
    rows += ['- PASS: ' + x['name'] for x in RESULT['checks']]
    if RESULT.get('error'):
        rows += ['', 'Failure: ' + RESULT['error']]
    rows += ['', 'No volumes were deleted. Test containers were stopped; CI host disposal owns temporary resource cleanup.',
             'The version-only rollback result applies only to this pair with matching schema fingerprints, data/key volumes and configuration.',
             'It does not validate arbitrary downgrades, real NAS backups, power failure or filesystem durability.']
    (OUT / 'REPORT.md').write_text('\n'.join(rows) + '\n')
    (OUT / 'SHA256SUMS').write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest() + '  ' + p.name + '\n'
        for p in sorted(OUT.iterdir()) if p.is_file() and p.name != 'SHA256SUMS'))


def extract_binary(archive, target):
    with tarfile.open(archive, 'r:gz') as t:
        members = [m for m in t.getmembers() if m.isfile() and Path(m.name).name == 'lantai']
        if len(members) != 1:
            raise RuntimeError('Unexpected CLI archive')
        target.write_bytes(t.extractfile(members[0]).read())
    target.chmod(0o700)


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--old-run', required=True)
    p.add_argument('--new-run', required=True)
    p.add_argument('--old-artifact', required=True)
    p.add_argument('--new-artifact', required=True)
    args = p.parse_args()
    work = Path(tempfile.mkdtemp(prefix='lantai-upgrade-'))
    os.chmod(work, 0o700)
    OUT.mkdir(exist_ok=True)
    repository = os.environ['GITHUB_REPOSITORY']
    RESULT['phase'] = 'download and verify release artifacts'
    manifests = {}
    for name, rid, artifact, commit in [('old', args.old_run, args.old_artifact, BASE),
                                         ('new', args.new_run, args.new_artifact, TARGET)]:
        dest = work / name
        run(['gh', 'run', 'download', rid, '--repo', repository, '--name', artifact, '--dir', str(dest)])
        manifest = json.loads((dest / 'manifest.json').read_text())
        check(name + ' manifest exact commit', manifest['source_commit'] == commit)
        for f in manifest['files']:
            path = dest / f['name']
            check_value = path.stat().st_size == f['size_bytes'] and hashlib.sha256(path.read_bytes()).hexdigest() == f['sha256']
            if not check_value:
                raise AssertionError(name + ' artifact hash mismatch')
        check(name + ' archive checksums verified', count=len(manifest['files']))
        manifests[name] = manifest
        run(['docker', 'load', '-i', str(dest / ('lantai-' + manifest['version'] + '-linux-amd64-image.tar.gz'))])
        if name == 'old':
            run(['docker', 'load', '-i', str(dest / 'caddy-2.11.4-linux-amd64-image.tar.gz')])
        extract_binary(dest / ('lantai-' + manifest['version'] + '-linux-amd64.tar.gz'), work / (name + '-lantai'))
    old, new = manifests['old'], manifests['new']
    check('distinct released core images', old['core_image']['id'] != new['core_image']['id'])
    RESULT['images'] = {'old': old['core_image'], 'new': new['core_image'], 'gateway': old['caddy_image']}
    # Existing helper validates image references and inspects without rebuilding.
    spec = importlib.util.spec_from_file_location('first_start', ROOT / 'scripts/test-compose-first-start.py')
    helper = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(helper)
    helper.select_images(old['core_image']['tag'], old['caddy_image']['tag'])
    helper.select_images(new['core_image']['tag'], old['caddy_image']['tag'])

    tls = work / 'tls'
    setup = work / 'setup'
    tls.mkdir(mode=0o700)
    setup.mkdir(mode=0o700)
    run(['openssl', 'req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes',
         '-days', '1', '-subj', '/CN=127.0.0.1', '-addext', 'subjectAltName=IP:127.0.0.1',
         '-keyout', str(tls / 'key.pem'), '-out', str(tls / 'cert.pem')])
    (tls / 'key.pem').chmod(0o600)
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    origin = 'https://127.0.0.1:' + str(port)
    config = work / 'config.yaml'
    config.write_text((ROOT / 'deploy/config.container.yaml').read_text().replace('https://library.example', origin)
                      .replace('min_free_bytes: 5368709120', 'min_free_bytes: 1048576'))
    config.chmod(0o644)
    config_hash = hashlib.sha256(config.read_bytes()).hexdigest()
    compose_file = work / 'compose.yaml'
    template = (ROOT / 'deploy/compose.yaml').read_text()
    assert template.count('image: caddy:2.11.4') == 1
    template = template.replace('image: caddy:2.11.4', 'image: ' + old['caddy_image']['tag'] + '\n    pull_policy: never')
    compose_file.write_text(template)
    (work / 'Caddyfile').write_text((ROOT / 'deploy/Caddyfile').read_text())
    env_file = work / 'compose.env'
    project_name = 'lantai-upgrade-' + secrets.token_hex(6)
    compose = ['docker', 'compose', '--project-name', project_name, '--env-file', str(env_file), '-f', str(compose_file)]

    def select(image):
        env_file.write_text('LANTAI_IMAGE=' + image + '\nLANTAI_SETUP_DIR=' + str(setup) +
            '\nLANTAI_CONFIG_FILE=' + str(config) + '\nLANTAI_TLS_DIR=' + str(tls) +
            '\nLANTAI_DOMAIN=127.0.0.1\nLANTAI_HTTPS_PORT=' + str(port) + '\n')
        env_file.chmod(0o600)

    context = ssl.create_default_context(cafile=str(tls / 'cert.pem'))
    token = None
    secrets_in_memory = []

    def request(path, payload=None, credential=None, idempotency_key=None):
        c = http.client.HTTPSConnection('127.0.0.1', port, context=context, timeout=8)
        headers = {'Origin': origin}
        if credential:
            headers['Authorization'] = 'Bearer ' + credential
        if idempotency_key:
            headers['Idempotency-Key'] = idempotency_key
        if payload is not None:
            headers['Content-Type'] = 'application/json'
        try:
            c.request('POST' if payload is not None else 'GET', path,
                      json.dumps(payload) if payload is not None else None, headers)
            r = c.getresponse()
            return r.status, r.read()
        finally:
            c.close()

    def wait(path, status):
        deadline = time.monotonic() + 75
        last = 'no response'
        while time.monotonic() < deadline:
            try:
                r = request(path)
                last = 'HTTP ' + str(r[0])
                if r[0] == status:
                    return r[1]
            except (OSError, http.client.HTTPException) as error:
                last = type(error).__name__ + ': ' + str(error)
            time.sleep(0.3)
        raise AssertionError('Expected HTTP ' + str(status) + ' for ' + path + '; last ' + last)

    def cli(binary, *args):
        output = run([str(binary), *args, '--server', origin, '--ca-file', str(tls / 'cert.pem'),
                      '--session-file', str(work / 'session.json')])
        return json.loads(output)

    def inspect(cid):
        return json.loads(run(['docker', 'inspect', cid]))[0]

    def mounts(cid):
        return {x['Destination']: {'type': x['Type'], 'source': x.get('Name', x['Source'])}
                for x in inspect(cid)['Mounts'] if x['Destination'] in ['/var/lib/lantai', '/var/lib/lantai-secrets']}

    def ops(cid, label):
        pid = str(inspect(cid)['State']['Pid'])
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            values = {}
            for endpoint in ['healthz', 'readyz']:
                p = subprocess.run(['sudo', 'nsenter', '--target', pid, '--net', '--', 'curl', '--noproxy', '*',
                    '--max-time', '8', '--silent', '--show-error', '--fail', 'http://127.0.0.1:9090/' + endpoint],
                    capture_output=True, text=True, env=ENV)
                if p.returncode == 0:
                    values[endpoint] = json.loads(p.stdout)
            if (values.get('healthz', {}).get('live') and values.get('readyz', {}).get('ready')
                    and values['readyz']['business_ready'] and values['readyz']['reason_codes'] == []):
                check(label + ' health and readiness', values=values)
                return
            time.sleep(0.5)
        raise AssertionError(label + ' health/readiness did not converge')

    def snapshot(cid, name):
        root = work / ('snapshot-' + name)
        data, keys = root / 'data', root / 'keys'
        data.mkdir(parents=True)
        keys.mkdir()
        run(['docker', 'cp', cid + ':/var/lib/lantai/.', str(data)])
        run(['docker', 'cp', cid + ':/var/lib/lantai-secrets/.', str(keys)])
        key_fingerprints = {str(p.relative_to(keys)): hashlib.sha256(p.read_bytes()).hexdigest()
                            for p in keys.rglob('*') if p.is_file()}
        schema, versions = {}, None
        for p in data.rglob('*.db'):
            con = sqlite3.connect('file:' + str(p) + '?mode=ro', uri=True)
            definitions = con.execute("SELECT type,name,tbl_name,sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name").fetchall()
            schema[str(p.relative_to(data))] = hashlib.sha256(json.dumps(definitions).encode()).hexdigest()
            if con.execute("SELECT 1 FROM sqlite_master WHERE name='ledger_versions'").fetchone():
                versions = con.execute('SELECT COUNT(*) FROM ledger_versions').fetchone()[0]
            con.close()
        check(name + ' snapshot contains databases and keys', len(schema) >= 5 and len(key_fingerprints) > 0)
        return {'schema': schema, 'keys': key_fingerprints, 'versions': versions}

    try:
        RESULT['phase'] = 'start rc2 and seed synthetic resource'
        select(old['core_image']['tag'])
        parsed_old = json.loads(run(compose + ['config', '--format', 'json']))
        # Also validate the legacy bind-directory configuration without starting it.
        legacy_env = dict(ENV, LANTAI_DATA_DIR=str(work / 'legacy-data'), LANTAI_SECRETS_DIR=str(work / 'legacy-keys'))
        legacy = json.loads(subprocess.check_output(compose + ['config', '--format', 'json'], env=legacy_env, text=True))
        for service in ['core', 'prepare']:
            v = {x['target']: x for x in legacy['services'][service]['volumes']}
            check(service + ' legacy bind overrides retained', v['/var/lib/lantai']['source'] == str(work / 'legacy-data')
                  and v['/var/lib/lantai-secrets']['source'] == str(work / 'legacy-keys'))
        run(compose + ['up', '-d'])
        try:
            wait('/setup/', 200)
        except Exception:
            # Before enrollment there are no user passwords, factors or sessions.
            # Still filter the synthetic key and all long token-like values.
            logs = subprocess.run(compose + ['logs', '--no-color', '--tail', '35'], capture_output=True, text=True, env=ENV)
            diagnostic = helper.redact_startup_log(logs.stdout + logs.stderr, (tls / 'key.pem').read_text())
            (OUT / 'startup-diagnostic.redacted.txt').write_text(diagnostic[:14000])
            print(diagnostic[:14000], flush=True)
            raise
        cid = run(compose + ['ps', '-q', 'core'])
        check('rc2 actual image ID', inspect(cid)['Image'] == old['core_image']['id'])
        claim = work / 'claim.json'
        run(['docker', 'cp', cid + ':/var/lib/lantai-setup/setup-code.json', str(claim)])
        claim.chmod(0o600)
        claim_code = json.loads(claim.read_text())['code']
        password = secrets.token_urlsafe(30)
        s, b = request('/setup/begin', {'name': 'upgrade-user', 'password': password}, claim_code)
        check('rc2 protected enrollment', s == 200)
        registration = json.loads(b)
        secret = base64.b32decode(registration['totp_secret'])
        secrets_in_memory.extend([claim_code, password, registration['totp_secret'], registration['enrollment_id'], *registration['recovery_codes']])

        last_totp_step = None

        def totp(fresh=False):
            nonlocal last_totp_step
            if fresh:
                while int(time.time()) // 30 == last_totp_step:
                    time.sleep(0.25)
            last_totp_step = int(time.time()) // 30
            d = hmac.new(secret, struct.pack('>Q', int(time.time()) // 30), hashlib.sha1).digest()
            offset = d[-1] & 15
            return str((struct.unpack('>I', d[offset:offset + 4])[0] & 0x7fffffff) % 1000000).zfill(6)

        s, _ = request('/setup/confirm', {'enrollment_id': registration['enrollment_id'], 'totp_code': totp(), 'recovery_codes_saved': True}, claim_code)
        check('rc2 administrator initialized', s == 200)
        meta = json.loads(wait('/api/v1/meta', 200))
        check('rc2 setup permanently closed', request('/setup/')[0] == 404)
        s, b = request('/api/v1/sessions/login', {'name': 'upgrade-user', 'password': password, 'code': totp(fresh=True),
                      'channel': 'cli', 'scopes': ['admin', 'read', 'ingest', 'organize', 'self'], 'ttl_seconds': 900})
        check('synthetic administrator session', s == 201)
        token = json.loads(b)['token']
        secrets_in_memory.append(token)
        (work / 'session.json').write_text(json.dumps({'schema': 'lantai.client-session/v1', 'origin': origin, 'token': token}))
        (work / 'session.json').chmod(0o600)
        old_cli, new_cli = work / 'old-lantai', work / 'new-lantai'
        who = cli(old_cli, 'whoami')
        (work / 'project.json').write_text(json.dumps({'key': 'upgrade-check', 'name': 'Synthetic upgrade', 'project_type': 'sandbox'}))
        project = cli(old_cli, 'project', 'create', '--input', str(work / 'project.json'), '--idempotency-key', 'synthetic-upgrade-project')
        # The system administrator does not silently gain a project's role.
        # Exercise the normal synthetic HumanGrant flow to seed this fixture.
        s, b = request('/api/v1/identity/projects/' + project['project_id'] + '/members', credential=token)
        check('synthetic membership revision readable', s == 200)
        membership = json.loads(b)
        intent = {'action': 'identity.grant_project_role', 'command': {
            'project_id': project['project_id'], 'expected_revision': membership['revision'],
            'principal_id': who['principal_id'], 'role': 'owner', 'grant': True}}
        s, b = request('/api/v1/identity/challenges', intent, token)
        check('synthetic project owner challenge', s == 201)
        challenge = json.loads(b)
        s, b = request('/api/v1/identity/challenges/' + challenge['challenge_id'] + '/verify',
                       {'code': totp(fresh=True)}, token)
        check('synthetic project owner proof', s == 201)
        grant = json.loads(b)
        s, _ = request('/api/v1/identity/commands', dict(intent, grant_id=grant['grant_id']), token,
                       idempotency_key='synthetic-upgrade-owner')
        check('synthetic project owner assigned', s == 200)
        inputs = work / 'input'
        inputs.mkdir()
        content = b'synthetic rc2 to rc3 upgrade retained resource\n'
        (inputs / 'sample.txt').write_bytes(content)
        (work / 'push.json').write_text(json.dumps({'project_id': project['project_id'], 'slug': 'upgrade-sample',
            'content': {'asset_type': 'doc', 'files': [{'path': 'sample.txt', 'role': 'source'}],
            'rights': {'usage': 'production', 'license': 'CC0-1.0', 'redistribute_raw': True, 'noai': False, 'sensitivity': 'normal'}}}))
        receipt = cli(old_cli, 'push', '--input', str(work / 'push.json'), '--directory', str(inputs), '--state', str(work / 'push-state.json'))
        check('rc2 synthetic resource committed', receipt['version_number'] == 1)
        cid = run(compose + ['ps', '-q', 'core'])
        old_mounts = mounts(cid)
        ops(cid, 'rc2')
        run(compose + ['stop'])
        before = snapshot(cid, 'before upgrade')

        RESULT['phase'] = 'upgrade to rc3 with same project and volumes'
        select(new['core_image']['tag'])
        parsed_new = json.loads(run(compose + ['config', '--format', 'json']))
        for service in ['core', 'prepare']:
            parsed_old['services'][service]['image'] = parsed_new['services'][service]['image']
        check('resolved Compose changes only core and prepare image', parsed_old == parsed_new)
        check('configuration file unchanged', hashlib.sha256(config.read_bytes()).hexdigest() == config_hash)
        run(compose + ['up', '-d'])
        current_meta = json.loads(wait('/api/v1/meta', 200))
        cid = run(compose + ['ps', '-q', 'core'])
        check('rc3 actual image ID', inspect(cid)['Image'] == new['core_image']['id'])
        check('same data and key volume identities', mounts(cid) == old_mounts)
        check('same instance identity', current_meta['instance_id'] == meta['instance_id'])
        check('setup remains closed after upgrade', request('/setup/')[0] == 404)
        check('same administrator session survives', cli(new_cli, 'whoami')['principal_id'] == who['principal_id'])
        ops(cid, 'rc3')
        cli(new_cli, 'show', '--asset', receipt['asset_id'], '--version', receipt['version_id'], '--view', 'full')
        output = work / 'download-new'
        output.mkdir()
        cli(new_cli, 'pull', '--asset', receipt['asset_id'], '--version', receipt['version_id'], '--directory', str(output), '--purpose', 'archive_review')
        check('pre-upgrade resource downloads unchanged', (output / 'sample.txt').read_bytes() == content,
              content_sha256=hashlib.sha256(content).hexdigest())
        replay = cli(new_cli, 'push', '--input', str(work / 'push.json'), '--directory', str(inputs), '--state', str(work / 'push-state.json'))
        check('original upload state replays same committed version', replay['asset_id'] == receipt['asset_id'] and replay['version_id'] == receipt['version_id'])
        run(compose + ['stop'])
        after = snapshot(cid, 'after upgrade')
        check('all database schema fingerprints unchanged', before['schema'] == after['schema'], database_count=len(before['schema']))
        check('key files unchanged', before['keys'] == after['keys'], file_count=len(before['keys']))
        check('ledger version count unchanged', before['versions'] == after['versions'] == 1)

        RESULT['phase'] = 'conditional image-only rollback for this schema-compatible pair'
        select(old['core_image']['tag'])
        run(compose + ['up', '-d'])
        rolled = json.loads(wait('/api/v1/meta', 200))
        cid = run(compose + ['ps', '-q', 'core'])
        check('rollback uses retained rc2 image', inspect(cid)['Image'] == old['core_image']['id'])
        check('rollback keeps volumes and instance', mounts(cid) == old_mounts and rolled['instance_id'] == meta['instance_id'])
        check('rollback does not reopen setup', request('/setup/')[0] == 404)
        check('administrator unchanged after rollback', cli(old_cli, 'whoami')['principal_id'] == who['principal_id'])
        ops(cid, 'rollback rc2')
        output = work / 'download-rollback'
        output.mkdir()
        cli(old_cli, 'pull', '--asset', receipt['asset_id'], '--version', receipt['version_id'], '--directory', str(output), '--purpose', 'archive_review')
        check('rollback retains exact resource bytes', (output / 'sample.txt').read_bytes() == content)
        logs = run(compose + ['logs', '--no-color'])
        check('ordinary logs exclude synthetic secrets', not any(s in logs for s in secrets_in_memory))
        RESULT['status'] = 'passed'
    finally:
        RESULT['phase'] = 'stop disposable test containers without deleting volumes'
        try:
            run(compose + ['stop'])
            check('test containers stopped; volumes preserved')
        except Exception:
            RESULT['cleanup'] = 'stop failed; GitHub runner disposal required'
        save()


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        RESULT['status'] = 'failed'
        RESULT['error'] = type(error).__name__ + ': ' + str(error)
        save()
        print('FAIL: ' + RESULT['error'], flush=True)
        raise SystemExit(1)
