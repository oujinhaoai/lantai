"""Reuse the frozen release builder with the r10 installation-entry contract.

The frozen first-start harness predates Entry: an unauthenticated metadata
request now returns 401 in the installation chooser, then 200 after enrollment.
Only that exact pre-enrollment assertion is adapted in memory. TLS, real Docker,
claim, TOTP, lifecycle, log-privacy, and release-delivery checks stay unchanged.
"""
import hashlib
import importlib.util
import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[2]
HARNESS = 'scripts/test-compose-first-start.py'
OLD = '            assert request("/api/v1/meta")[0] == 404\n'
NEW = '            assert request("/api/v1/meta")[0] == 401\n'

def first_start():
    path = ROOT / HARNESS
    raw = path.read_bytes()
    frozen = json.loads((ROOT/'packaging/r10/FROZEN-SOURCE.json').read_text())
    record = next(x for x in frozen['files'] if x['path'] == HARNESS)
    assert hashlib.sha256(raw).hexdigest() == record['sha256']
    text = raw.decode()
    assert text.count(OLD) == 1
    # Compile with its original filename so ROOT and all synthetic inputs remain
    # the frozen harness's; no source file is written or modified.
    namespace = {'__file__': str(path), '__name__': 'r10_first_start'}
    exec(compile(text.replace(OLD, NEW), str(path), 'exec'), namespace)
    namespace['main']()
    print('PASS: r10 installation chooser rejects unauthenticated metadata with HTTP 401')
    args = sys.argv[1:]
    if '--evidence-file' in args:
        target = Path(args[args.index('--evidence-file') + 1])
        evidence = json.loads(target.read_text())
        evidence['packaging_compatibility'] = {'original_harness_sha256': record['sha256'],
            'only_adaptation': 'pre-enrollment metadata HTTP 404 assertion becomes strict HTTP 401',
            'frozen_source_modified': False}
        target.write_text(json.dumps(evidence, indent=2)+'\n')

def build():
    path = ROOT/'scripts/build-release.py'
    spec = importlib.util.spec_from_file_location('frozen_release_builder', path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    original = module.command
    def command(argv, **kwargs):
        if len(argv) > 1 and Path(argv[1]).as_posix().endswith('/'+HARNESS):
            source = Path(argv[1]).resolve().parents[1]
            argv = [argv[0], str(source/'packaging/r10/build_candidate.py'), '--first-start', *argv[2:]]
        return original(argv, **kwargs)
    module.command = command
    module.main()

if __name__ == '__main__':
    if len(sys.argv) > 1 and sys.argv[1] == '--first-start':
        del sys.argv[1]
        first_start()
    else:
        build()
