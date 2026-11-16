#!/usr/bin/env python3
"""Disposable no-Docker macOS HTTPS smoke. Never accepts an existing data root."""
import argparse
import base64
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import selectors
import socket
import ssl
import struct
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def stop_all(processes):
    failed = False
    for process in reversed(processes):
        try:
            stop(process)
        except (OSError, subprocess.TimeoutExpired):
            failed = True
    if failed:
        raise RuntimeError('Could not confirm all child processes stopped')


def opener(context=None):
    # Explicitly bypass all inherited proxies, including the first readiness probe.
    return urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                     urllib.request.HTTPSHandler(context=context))


def fetch(client, url):
    try:
        with client.open(url, timeout=2) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, b''


def require_resource(client, url, expected):
    status, body = fetch(client, url)
    if status != 200 or body != expected:
        raise RuntimeError('HTTP resource status or bytes mismatch')


def wait_ready(client, url, process):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError('Service exited before readiness')
        try:
            if fetch(client, url)[0] == 200:
                return
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(.2)
    raise RuntimeError('Service readiness timed out')


def check_listeners(output, ports):
    addresses = re.findall(r'TCP\s+(\S+)\s+\(LISTEN\)', output)
    if sorted(addresses) != sorted(f'127.0.0.1:{p}' for p in ports):
        raise RuntimeError('Unexpected or missing listener')


def initialize(binary, home, env):
    # Only a freshly created synthetic instance. Never emit password, seed or codes.
    process = subprocess.Popen([str(binary), 'init', '--home', str(home),
                                '--admin', 'synthetic-admin'], stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=env)
    try:
        password = secrets.token_urlsafe(30)
        process.stdin.write((password + '\n' + password + '\n').encode())
        process.stdin.flush()
        output = b''
        deadline = time.monotonic() + 30
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            while time.monotonic() < deadline:
                if not selector.select(timeout=.2):
                    continue
                chunk = os.read(process.stdout.fileno(), 4096)
                if not chunk:
                    break
                output += chunk
                if len(output) > 65536:
                    break
                match = re.search(rb'  secret  ([A-Z2-7]+)', output)
                if match:
                    seed = match.group(1)
                    key = base64.b32decode(seed + b'=' * ((-len(seed)) % 8))
                    digest = hmac.new(key, struct.pack('>Q', int(time.time()) // 30),
                                      hashlib.sha1).digest()
                    offset = digest[-1] & 15
                    code = (struct.unpack('>I', digest[offset:offset + 4])[0]
                            & 0x7fffffff) % 1000000
                    process.communicate(f'{code:06d}\n'.encode(), timeout=30)
                    if process.returncode != 0:
                        raise RuntimeError('Synthetic initialization failed')
                    return
        raise RuntimeError('Synthetic initialization timed out or failed')
    finally:
        stop(process)


def run(binary, caddy, web):
    if sys.platform != 'darwin':
        raise RuntimeError('This listener-verified harness requires macOS')
    if not (web / 'index.html').is_file():
        raise RuntimeError('Build web/dist first')
    assets = sorted((web / 'assets').rglob('*'))
    files = [web / 'index.html'] + [p for p in assets if p.is_file()]
    if len(files) < 2 or any(p.is_symlink() for p in [web, web / 'index.html', web / 'assets'] + assets):
        raise RuntimeError('Expected built assets without symlinks')
    env = {k: os.environ[k] for k in ('PATH', 'TMPDIR') if k in os.environ}
    with tempfile.TemporaryDirectory(prefix='lantai-native-smoke-') as directory:
        work = Path(directory)
        home, sec, tls = [work / name for name in ('data', 'secrets', 'tls')]
        for path in (home, sec, tls):
            path.mkdir(mode=0o700)
        # Keep sockets reserved until all four distinct ports have been allocated.
        sockets = [socket.socket() for _ in range(4)]
        try:
            for sock in sockets:
                sock.bind(('127.0.0.1', 0))
            ports = [sock.getsockname()[1] for sock in sockets]
        finally:
            for sock in sockets:
                sock.close()
        api, xfer, ops, https = ports
        (home / 'config.yaml').write_text(
            'contract: lantai.config/v1\ninstance:\n  name: native-smoke\n'
            f'secrets:\n  dir: {json.dumps(str(sec))}\nstorage:\n  min_free_bytes: 1048576\n'
            f'listen:\n  api: 127.0.0.1:{api}\n  transfer: 127.0.0.1:{xfer}\n'
            f'  operations: 127.0.0.1:{ops}\n  merged: false\nhttp:\n'
            f'  allowed_origins:\n    - https://localhost:{https}\n')
        env.update(HOME=str(work), XDG_DATA_HOME=str(work / 'caddy-data'),
                   XDG_CONFIG_HOME=str(work / 'caddy-config'))
        initialize(binary, home, env)
        cert, key = tls / 'cert.pem', tls / 'key.pem'
        config = tls / 'openssl.cnf'
        config.write_text('[req]\ndistinguished_name=dn\nx509_extensions=ext\nprompt=no\n'
                          '[dn]\nCN=localhost\n[ext]\nsubjectAltName=DNS:localhost,IP:127.0.0.1\n'
                          'basicConstraints=critical,CA:TRUE\n'
                          'keyUsage=critical,digitalSignature,keyEncipherment,keyCertSign\n')
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
                        '-keyout', str(key), '-out', str(cert), '-days', '1',
                        '-config', str(config)], env=env, check=True, timeout=30,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        key.chmod(0o600)
        env.update(LANTAI_HTTPS_ADDRESS=f'https://localhost:{https}',
                   LANTAI_GATEWAY_BIND='127.0.0.1', LANTAI_TLS_CERT=str(cert),
                   LANTAI_TLS_KEY=str(key), LANTAI_API_UPSTREAM=f'127.0.0.1:{api}',
                   LANTAI_TRANSFER_UPSTREAM=f'127.0.0.1:{xfer}', LANTAI_WEB_ROOT=str(web))
        processes = []
        try:
            core = subprocess.Popen([str(binary), 'serve', '--home', str(home)], env=env,
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            processes.append(core)
            wait_ready(opener(), f'http://127.0.0.1:{ops}/readyz', core)
            command = [str(caddy), 'run', '--config', str(ROOT / 'deploy/Caddyfile'),
                       '--adapter', 'caddyfile']
            gateway = subprocess.Popen(command, env=env, stdout=subprocess.DEVNULL,
                                       stderr=subprocess.DEVNULL)
            processes.append(gateway)
            client = opener(ssl.create_default_context(cafile=str(cert)))
            base = f'https://localhost:{https}'
            wait_ready(client, base + '/api/v1/meta', gateway)
            listeners = subprocess.check_output(
                ['lsof', '-nP', '-a', '-p', f'{core.pid},{gateway.pid}', '-iTCP',
                 '-sTCP:LISTEN'], text=True, timeout=10)
            check_listeners(listeners, ports)
            for path in files:
                route = '/' if path.name == 'index.html' and path.parent == web else (
                    '/' + urllib.parse.quote(path.relative_to(web).as_posix()))
                require_resource(client, base + route, path.read_bytes())
            for route in ('/healthz', '/readyz', '/setup/'):
                if fetch(client, base + route)[0] != 404:
                    raise RuntimeError('Gateway exposed a forbidden route')
        finally:
            stop_all(processes)
        subprocess.run([str(binary), 'doctor', '--home', str(home), '-json'], env=env,
                       check=True, timeout=30, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL)
    return {'status': 'passed', 'tls_verified': True, 'loopback_only': True,
            'resources_checked': len(files), 'temporary_instance_removed': True,
            'authenticated_business_flow_tested': False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--lantai', type=Path, required=True)
    parser.add_argument('--caddy', type=Path, required=True)
    parser.add_argument('--web-dist', type=Path, default=ROOT / 'web/dist')
    args = parser.parse_args()
    try:
        print(json.dumps(run(args.lantai.resolve(), args.caddy.resolve(),
                             args.web_dist.resolve())))
    except Exception as error:
        # Suppress raw child output/paths, which may contain generated credentials.
        print(f'Native smoke failed ({type(error).__name__})', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
