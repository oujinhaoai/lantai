#!/usr/bin/env python3
"""Run Linux browser tests with a private, process-scoped synthetic CA.

The host CA file is never written. A private mount namespace gives WebKit's
system TLS backend the test bundle; Chromium uses a disposable NSS user DB.
Browser TLS verification and Chromium's sandbox remain enabled.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import pwd
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
HOST_CA = Path('/etc/ssl/certs/ca-certificates.crt')


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def child(config_path):
    config = json.loads(Path(config_path).read_text())
    if os.geteuid() != 0 or config['uid'] == 0:
        raise RuntimeError('A non-root test user and isolated mount setup are required')
    if os.readlink('/proc/self/ns/mnt') == config['host_namespace']:
        raise RuntimeError('Refusing to change trust in the host mount namespace')
    subprocess.run(['/usr/bin/mount', '--bind', config['bundle'], str(HOST_CA)], check=True)
    subprocess.run(['/usr/bin/mount', '-o', 'remount,bind,ro', str(HOST_CA)], check=True)
    os.initgroups(pwd.getpwuid(config['uid']).pw_name, config['gid'])
    os.setgid(config['gid'])
    os.setuid(config['uid'])
    os.chdir(ROOT / 'web')
    os.execvpe('npm', ['npm', 'test'], config['env'])


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--child')
    parser.add_argument('--evidence-file', type=Path)
    args = parser.parse_args()
    if args.child:
        child(args.child)
        return
    if sys.platform != 'linux' or os.geteuid() == 0:
        raise RuntimeError('This harness requires Linux and a non-root CI user')
    before = digest(HOST_CA)
    work = Path(tempfile.mkdtemp(prefix='lantai-web-strict-'))
    result = {'schema': 'lantai.browser-ci-trust/v1', 'status': 'failed',
              'host_ca_sha256_before': before, 'host_trust_store_changed': False,
              'ignore_https_errors': False, 'chromium_sandbox': True}
    try:
        tls = work / 'tls'
        generated = subprocess.check_output(
            ['node', str(ROOT / 'web/tests/create-test-tls.mjs'), str(tls)], text=True)
        env = {key: os.environ[key] for key in ('PATH', 'CI', 'GOTOOLCHAIN', 'GOROOT',
               'GOCACHE', 'GOMODCACHE', 'GOPATH', 'TMPDIR',
               'LANTAI_WEB_CHROMIUM_EXECUTABLE') if key in os.environ}
        env.update(json.loads(generated))
        home = work / 'home'
        database = home / '.pki/nssdb'
        database.mkdir(parents=True, mode=0o700)
        subprocess.run(['certutil', '-N', '-d', 'sql:' + str(database),
                        '--empty-password'], check=True)
        subprocess.run(['certutil', '-A', '-d', 'sql:' + str(database), '-n',
                        'Lantai synthetic browser CA', '-t', 'C,,', '-i',
                        env['LANTAI_WEB_CA_FILE']], check=True)
        result['nss_tools_version'] = subprocess.check_output(
            ['dpkg-query', '-W', '-f=${Version}', 'libnss3-tools'], text=True).strip()
        for name in ('config', 'data', 'cache'):
            (home / name).mkdir(mode=0o700)
        env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / 'config'),
                   XDG_DATA_HOME=str(home / 'data'), XDG_CACHE_HOME=str(home / 'cache'),
                   PLAYWRIGHT_BROWSERS_PATH=os.environ.get('PLAYWRIGHT_BROWSERS_PATH',
                       str(Path.home() / '.cache/ms-playwright')))
        bundle = work / 'ca-bundle.pem'
        bundle.write_bytes(HOST_CA.read_bytes() + b'\n' + Path(env['LANTAI_WEB_CA_FILE']).read_bytes())
        bundle.chmod(0o644)  # Public certificates; private key files remain 0600.
        result['synthetic_ca_sha256'] = digest(Path(env['LANTAI_WEB_CA_FILE']))
        # The negative browser cases use a second, explicitly untrusted CA.
        untrusted = work / 'untrusted'
        subprocess.check_output(['node', str(ROOT / 'web/tests/create-test-tls.mjs'),
                                 str(untrusted)], text=True)
        env['LANTAI_WEB_UNTRUSTED_CERT'] = str(untrusted / 'localhost.pem')
        env['LANTAI_WEB_UNTRUSTED_KEY'] = str(untrusted / 'localhost-key.pem')
        wrong = tls / 'wrong-host'
        command = ['openssl', 'req', '-new', '-newkey', 'rsa:2048', '-nodes',
                   '-subj', '/CN=wrong-host.invalid', '-keyout', str(wrong) + '.key',
                   '-out', str(wrong) + '.csr']
        subprocess.run(command, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        ext = tls / 'wrong-host.ext'
        ext.write_text('subjectAltName=DNS:wrong-host.invalid\nextendedKeyUsage=serverAuth\n')
        subprocess.run(['openssl', 'x509', '-req', '-in', str(wrong) + '.csr',
                        '-CA', str(tls / 'ca.pem'), '-CAkey', str(tls / 'ca-key.pem'),
                        '-CAcreateserial', '-days', '1', '-sha256', '-extfile', str(ext),
                        '-out', str(wrong) + '.pem'], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        Path(str(wrong) + '.key').chmod(0o600)
        env['LANTAI_WEB_WRONG_HOST_CERT'] = str(wrong) + '.pem'
        env['LANTAI_WEB_WRONG_HOST_KEY'] = str(wrong) + '.key'
        config = work / 'child.json'
        config.write_text(json.dumps({'uid': os.getuid(), 'gid': os.getgid(),
            'host_namespace': os.readlink('/proc/self/ns/mnt'),
            'bundle': str(bundle), 'env': env}))
        config.chmod(0o600)
        process = subprocess.run(['/usr/bin/sudo', '--non-interactive', '/usr/bin/unshare', '--mount',
            '--propagation', 'private', '--', sys.executable, str(Path(__file__).resolve()),
            '--child', str(config)])
        result['exit_code'] = process.returncode
        result['status'] = 'passed' if process.returncode == 0 else 'failed'
    finally:
        result['host_ca_sha256_after'] = digest(HOST_CA)
        result['host_trust_store_changed'] = result['host_ca_sha256_after'] != before
        shutil.rmtree(work)
        if args.evidence_file:
            args.evidence_file.write_text(json.dumps(result, indent=2) + '\n')
        print(json.dumps(result), flush=True)
    if result['host_trust_store_changed'] or result.get('exit_code') != 0:
        raise SystemExit('Strict browser tests failed; see scoped test evidence')


if __name__ == '__main__':
    main()
