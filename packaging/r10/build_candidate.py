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
import tempfile
import shutil

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

def browser_preflight():
    path=Path('/opt/google/chrome/chrome')
    result={'schema':'lantai.safe-browser-preflight/v1','status':'failed','host_policy_changed':False,
        'sandbox_required':True,'strict_TLS_required':True,'candidates':[]}
    if path.is_file():
        info=path.stat()
        item={'path':str(path),'owner_uid':info.st_uid,'mode':oct(info.st_mode & 0o7777),
            'sha256':hashlib.sha256(path.read_bytes()).hexdigest()}
        package=subprocess.run(['dpkg-query','-S',str(path)],text=True,capture_output=True)
        item['installed_package']=package.stdout.strip()
        inventory=Path('/var/lib/dpkg/info/google-chrome-stable.md5sums')
        checksum=None
        if inventory.is_file() and inventory.stat().st_uid==0 and inventory.stat().st_mode & 0o022==0:
            for line in inventory.read_text().splitlines():
                fields=line.split()
                if len(fields)==2 and fields[1]=='opt/google/chrome/chrome':checksum=fields[0]
        item['installed_package_integrity_verified']=(package.returncode==0 and
            package.stdout.startswith('google-chrome-stable: ') and checksum is not None and
            hashlib.md5(path.read_bytes()).hexdigest()==checksum)
        result['candidates'].append(item)
        if item['installed_package_integrity_verified']:
            # Browser ownership is not a sandbox guarantee. Verify the installed
            # package bytes, then ask Chromium for actual namespace/seccomp state.
            # Never edit the binary/helper or any host security policy.
            code=r'''
const fs=require('fs'),os=require('os'),path=require('path');
const {chromium}=require('./web/node_modules/@playwright/test');
(async()=>{const profile=fs.mkdtempSync(path.join(os.tmpdir(),'r10-browser-preflight-'));fs.chmodSync(profile,0o700);
let c,state={passed:false};try{c=await chromium.launchPersistentContext(profile,{executablePath:'/opt/google/chrome/chrome',chromiumSandbox:true,ignoreHTTPSErrors:false});
const p=await c.newPage();await p.goto('chrome://sandbox');const report=await p.locator('body').innerText();
if(!/PID namespaces\s+Yes/i.test(report)||!/Seccomp-BPF sandbox\s+Yes/i.test(report))throw new Error('Chromium namespace/seccomp sandbox proof unavailable');
state={passed:false,version:c.browser().version(),sandbox_verified:true,sandbox_report:report};
await p.goto('chrome://certificate-manager/localcerts');state.native_CA_manager_text=(await p.locator('body').innerText()).slice(0,3000);
try{await p.locator('certificate-list-v2').filter({hasText:/Trusted certificates/i}).waitFor({state:'visible',timeout:15000})}catch(e){state.error_type=e.name;state.message=e.message;console.log(JSON.stringify(state));return}
console.log(JSON.stringify({passed:true,version:c.browser().version(),sandbox_report:report,native_profile_CA_manager_available:true}));
}finally{if(c)await c.close();fs.rmSync(profile,{recursive:true,force:true});}})().catch(e=>{console.log(JSON.stringify({passed:false,error_type:e.name,message:e.message}));process.exitCode=1});
'''
            try:probe=subprocess.run(['node','-e',code],cwd=ROOT,text=True,capture_output=True,timeout=90)
            except subprocess.TimeoutExpired:
                item['probe']={'passed':False,'error_type':'BrowserPreflightTimeout'}
                Path(argument('--out')).write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
                print('BROWSER_PREFLIGHT '+json.dumps(result,ensure_ascii=False),flush=True)
                return
            try:item['probe']=json.loads(probe.stdout.strip().splitlines()[-1])
            except (ValueError,IndexError):item['probe']={'passed':False,'error_type':'BrowserPreflightProtocolError'}
            if item['probe'].get('sandbox_verified'):
                result.update(sandbox_status='passed',selected_executable=str(path),native_profile_CA_manager_status='failed')
            if probe.returncode==0 and item['probe'].get('passed'):
                result.update(status='passed',sandbox_status='passed',native_profile_CA_manager_status='passed',selected_executable=str(path))
    Path(argument('--out')).write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
    print('BROWSER_PREFLIGHT '+json.dumps(result,ensure_ascii=False),flush=True)

def delivery():
    module = load_script('frozen_delivery_smoke', 'test-release-delivery.py')
    manifest = json.loads(Path(argument('--manifest')).read_text())
    candidate_tag = manifest['core_image']['tag']
    proof=Path(os.environ['RUNNER_TEMP'])/'r10-browser-preflight.json'
    browser=json.loads(proof.read_text())
    module.RESULT['browser_preflight']=browser
    if browser.get('sandbox_status')=='passed':
        assert browser['selected_executable']=='/opt/google/chrome/chrome'
        module.ENV['LANTAI_WEB_CHROMIUM_EXECUTABLE']=browser['selected_executable']
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
    # This candidate deliverable targets Linux amd64. Keep the existing builder
    # and every related acceptance check; do not cross-build unused platforms.
    module.PLATFORMS=[('linux','amd64')]
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
        if (result.get('status')!='failed' or not (output/'build-work/candidate-manifest.json').is_file()):
            raise
        candidate = json.loads((output/'build-work/candidate-manifest.json').read_text())
        lifecycle = json.loads((output/'deliverables/first-start-lifecycle.json').read_text())
        assert lifecycle['status']=='passed'
        assert lifecycle['core_image_id']==candidate['core_image']['id']
        assert lifecycle['caddy_image_id']==candidate['caddy_image']['id']
        assert result['source_commit']==argument('--commit') and result['core_image_id']==candidate['core_image']['id']
        candidate.update(schema='lantai.release-artifacts/v1',
            status='built-with-acceptance-failure', source_commit=argument('--commit'),
            go_version=original(['go','version']), delivery_smoke=result,
            first_start_smoke={'status':'passed','core_image_id':lifecycle['core_image_id'],
                'caddy_image_id':lifecycle['caddy_image_id'],'scope':'synthetic first start; not NAS acceptance'})
        (output/'deliverables/manifest.json').write_text(json.dumps(candidate, ensure_ascii=False, indent=2)+'\n')
        (output/'deliverables/delivery-image-smoke.txt').write_text('Acceptance failed. See delivery-image-smoke.json for separate browser, upgrade, cleanup and successful checks. Do not treat this package as an approved in-place upgrade.\n')
        print('BUILT_WITH_ACCEPTANCE_FAILURE: downloadable candidate must not be reported as fully accepted or safe to upgrade', flush=True)
    proof=Path(os.environ['RUNNER_TEMP'])/'r10-browser-preflight.json'
    shutil.copyfile(proof,Path(argument('--out'))/'deliverables/browser-sandbox-preflight.json')

if __name__ == '__main__':
    if len(sys.argv) > 1 and sys.argv[1] == '--first-start':
        del sys.argv[1]
        first_start()
    elif len(sys.argv) > 1 and sys.argv[1] == '--delivery':
        del sys.argv[1]
        delivery()
    elif len(sys.argv) > 1 and sys.argv[1] == '--browser-preflight':
        del sys.argv[1]
        browser_preflight()
    else:
        build()
