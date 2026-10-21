"""Reuse the frozen release builder with the r10 installation-entry contract.

The frozen first-start harness predates Entry: an unauthenticated metadata
request now returns 401 in the installation chooser, then 200 after enrollment.
Only that exact pre-enrollment assertion is adapted in memory. TLS, real Docker,
claim, TOTP, lifecycle, log-privacy, and release-delivery checks stay unchanged.
"""
import hashlib
import importlib.util
import json
import os
from copy import deepcopy
from pathlib import Path
import subprocess
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

def load_script(name, filename):
    path = ROOT/'scripts'/filename
    sys.path.insert(0, str(path.parent))
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

def argument(name):
    return sys.argv[sys.argv.index(name)+1]

def delivery():
    module = load_script('frozen_delivery_smoke', 'test-release-delivery.py')
    manifest = json.loads(Path(argument('--manifest')).read_text())
    candidate_tag = manifest['core_image']['tag']
    # Use the runner's preinstalled full browser under its existing OS policy.
    # Do not install/setuid/chown anything or relax AppArmor/userns restrictions.
    system_browser = Path('/opt/google/chrome/chrome')
    if system_browser.is_file():
        info = system_browser.stat()
        assert info.st_uid == 0 and info.st_mode & 0o022 == 0 and os.access(system_browser, os.X_OK)
        helper = system_browser.with_name('chrome-sandbox')
        helper_info = helper.stat() if helper.is_file() else None
        if helper_info:
            assert helper_info.st_uid == 0 and helper_info.st_mode & 0o022 == 0
        module.ENV['LANTAI_WEB_CHROMIUM_EXECUTABLE'] = str(system_browser)
        module.RESULT['browser_executable'] = {'path': str(system_browser), 'owner_uid':info.st_uid,
            'mode': oct(info.st_mode & 0o7777), 'sha256': hashlib.sha256(system_browser.read_bytes()).hexdigest(),
            'version': subprocess.check_output([str(system_browser),'--version'], text=True).strip(),
            'sandbox_helper_owner_uid': helper_info.st_uid if helper_info else None,
            'sandbox_helper_mode': oct(helper_info.st_mode & 0o7777) if helper_info else None,
            'sandbox_enabled':True,'strict_TLS':True,'host_policy_changed':False}
    original_select = module.Instance.select
    def select(instance, image):
        if image == candidate_tag:
            path = instance.root/'compose.yaml'
            text = path.read_text()
            old = 'command: [start, --home, /var/lib/lantai, --setup-dir, /var/lib/lantai-setup]'
            new = old[:-1]+', --config-template, /etc/lantai/config.template.yaml]'
            legacy = ':/var/lib/lantai/config.yaml:ro'
            if legacy in text:
                assert text.count(old)==1 and text.count(legacy)==1
                path.write_text(text.replace(old,new).replace(legacy,':/etc/lantai/config.template.yaml:ro'))
                module.RESULT['legacy_config_template_upgrade']={'applied':True,
                    'host_config_source_retained':True,'data_secrets_setup_volumes_retained':True,
                    'read_only_template_target':'/etc/lantai/config.template.yaml',
                    'only_command_addition':['--config-template','/etc/lantai/config.template.yaml']}
        return original_select(instance,image)
    module.Instance.select = select
    original_guard = module.assert_retained_upgrade
    def guard(before, after, **kwargs):
        normalized = deepcopy(after)
        old_core = before['services']['core']
        new_core = normalized['services']['core']
        old_mount = next(x for x in old_core['volumes'] if x['target']=='/var/lib/lantai/config.yaml')
        new_mount = next(x for x in new_core['volumes'] if x['target']=='/etc/lantai/config.template.yaml')
        assert old_mount['read_only'] and new_mount['read_only']
        expected = dict(old_mount, target='/etc/lantai/config.template.yaml')
        assert new_mount == expected
        assert new_core['command']==old_core['command']+['--config-template','/etc/lantai/config.template.yaml']
        new_mount['target']=old_mount['target']
        new_core['command']=old_core['command']
        return original_guard(before,normalized,**kwargs)
    module.assert_retained_upgrade = guard
    original_mounts = module.Instance.mounts
    def mounts(instance):
        result = original_mounts(instance)
        if '/var/lib/lantai/config.yaml' not in result:
            bound = [x for x in module.inspect(instance.cid())['Mounts'] if x['Destination']=='/etc/lantai/config.template.yaml']
            assert len(bound)==1 and not bound[0]['RW']
            item=bound[0]
            result['/var/lib/lantai/config.yaml']={'type':item['Type'],'source':item.get('Name',item['Source'])}
        return result
    module.Instance.mounts = mounts
    original_wait = module.Instance.wait
    def wait(instance,path,status):
        try:
            return original_wait(instance,path,status)
        except Exception:
            # Synthetic core logs only, bounded and redacted before export.
            log=subprocess.run(instance.compose+['logs','--no-color','--tail','30','core'],
                capture_output=True,text=True,env=module.ENV).stdout
            for value in module.PRIVATE_VALUES:
                if value:log=log.replace(value,'[REDACTED]')
            module.RESULT['failed_wait']={'path':path,'expected_status':status,'core_log_redacted':log[-5000:]}
            try:
                module.RESULT['failed_wait']['last_http_status']=instance.request(path)[0]
            except Exception as probe_error:
                module.RESULT['failed_wait']['request_error_type']=type(probe_error).__name__
            raise
    module.Instance.wait = wait
    original = module.Instance.browser
    failures = []
    def browser(instance, evidence, label):
        try:
            return original(instance, evidence, label)
        except Exception as error:
            # The browser subprocess already redacts PRIVATE_VALUES. Keep its
            # failure explicit, then independently exercise backup/restart/
            # upgrade/rollback. Never change TLS, browser sandbox, or host policy.
            failures.append({'label': label, 'status': 'failed',
                'error_type': type(error).__name__,
                'diagnostic': module.RESULT.pop('command_stderr_redacted', '')})
            module.RESULT.pop('failed_command', None)
    out = Path(argument('--out'))/'delivery-image-smoke.json'
    module.Instance.browser = browser
    try:
        module.main()
    except Exception as error:
        message = str(error)
        for value in module.PRIVATE_VALUES:
            if value:
                message = message.replace(value, '[REDACTED]')
        module.RESULT.update(status='failed', error=type(error).__name__+': '+message,
            browser_failures=failures, nonbrowser_checks_status='failed')
        for failure in failures:
            key='fresh_install_passed' if failure['label']=='fresh' else 'direct_upgrade_passed'
            if module.RESULT.get(key):
                module.RESULT[key.replace('_passed','_nonbrowser_checks_completed')]=True
            module.RESULT[key]=False
        out.write_text(json.dumps(module.RESULT, ensure_ascii=False, indent=2)+'\n')
        print('DELIVERY_FAILURE_SUMMARY '+json.dumps(module.RESULT, ensure_ascii=False), flush=True)
        raise SystemExit(1)
    result = json.loads(out.read_text())
    result['nonbrowser_checks_status'] = 'passed'
    result['browser_failures'] = failures
    if failures:
        result['status'] = 'failed'
        result['fresh_nonbrowser_checks_completed'] = result['fresh_install_passed']
        result['direct_upgrade_nonbrowser_checks_completed'] = result['direct_upgrade_passed']
        for failure in failures:
            result['fresh_install_passed' if failure['label']=='fresh' else 'direct_upgrade_passed'] = False
    out.write_text(json.dumps(result, ensure_ascii=False, indent=2)+'\n')
    print('DELIVERY_BROWSER_RESULT '+json.dumps({'nonbrowser_checks_status': 'passed', 'browser_failures': failures}))

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
        elif len(argv) > 1 and Path(argv[1]).name == 'test-release-delivery.py':
            source = Path(argv[1]).resolve().parents[1]
            argv = [argv[0], str(source/'packaging/r10/build_candidate.py'), '--delivery', *argv[2:]]
        return original(argv, **kwargs)
    module.command = command
    try:
        module.main()
    except Exception as error:
        output = Path(argument('--out'))
        report = output/'deliverables/delivery-image-smoke.json'
        result = json.loads(report.read_text()) if report.exists() else {}
        # Never infer a pass from a swallowed stdout stream. Emit only the
        # intentionally public, synthetic smoke report on an unsuccessful gate.
        print('DELIVERY_FAILURE_SUMMARY '+json.dumps(result, ensure_ascii=False), flush=True)
        (output/'delivery-failure-summary.json').write_text(json.dumps(result, ensure_ascii=False, indent=2)+'\n')
        if (str(error) != 'actual delivery image/static/direct-upgrade smoke did not pass'
                or result.get('nonbrowser_checks_status') != 'passed'
                or not result.get('browser_failures')):
            raise
        candidate = json.loads((output/'build-work/candidate-manifest.json').read_text())
        lifecycle = json.loads((output/'deliverables/first-start-lifecycle.json').read_text())
        assert lifecycle['status']=='passed'
        assert lifecycle['core_image_id']==candidate['core_image']['id']
        assert lifecycle['caddy_image_id']==candidate['caddy_image']['id']
        candidate.update(schema='lantai.release-artifacts/v1',
            status='built-with-browser-acceptance-failure', source_commit=argument('--commit'),
            go_version=original(['go','version']), delivery_smoke=result,
            first_start_smoke={'status':'passed','core_image_id':lifecycle['core_image_id'],
                'caddy_image_id':lifecycle['caddy_image_id'],'scope':'synthetic first start; not NAS acceptance'})
        (output/'deliverables/manifest.json').write_text(json.dumps(candidate, ensure_ascii=False, indent=2)+'\n')
        (output/'deliverables/delivery-image-smoke.txt').write_text('Browser acceptance failed; non-browser checks completed. See delivery-image-smoke.json.\n')
        print('BUILT_WITH_BROWSER_ACCEPTANCE_FAILURE: downloadable candidate must not be reported as fully accepted', flush=True)

if __name__ == '__main__':
    if len(sys.argv) > 1 and sys.argv[1] == '--first-start':
        del sys.argv[1]
        first_start()
    elif len(sys.argv) > 1 and sys.argv[1] == '--delivery':
        del sys.argv[1]
        delivery()
    else:
        build()
