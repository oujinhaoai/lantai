"""Repackage verified immutable bytes with guarded GUI configuration and evidence."""
import argparse,hashlib,json,os,re,shutil
from pathlib import Path
ROOT=Path(__file__).resolve().parents[2]
SOURCE='3ad86ea10830676d5ef5d72286a02fd0d9d01c65'
IMAGE='sha256:38d2114be464e0ba333f2bf4b0a3e88cfbd41bed5b474728e2513df644034945'
WEB='0c21322ab7506562dc0dc32e53c63b51aef6fd48184a93127ca9f3fd40707dfd'
def sha(p):
    with p.open('rb') as f:return hashlib.file_digest(f,'sha256').hexdigest()
def save(p,v):p.write_text(json.dumps(v,ensure_ascii=False,indent=2)+'\n')
def find(root,name):
    values=list(root.rglob(name));assert len(values)==1,name;return values[0]
def main():
    ap=argparse.ArgumentParser()
    for key in ['candidate','migration','browser','isolation','proof','out']:ap.add_argument('--'+key,type=Path,required=True)
    a=ap.parse_args();m=json.loads((a.candidate/'manifest.json').read_text())
    for line in (a.candidate/'SHA256SUMS').read_text().splitlines():
        digest,name=line.split('  ',1);assert '/' not in name and sha(a.candidate/name)==digest
    assert m['source_commit']==SOURCE and m['core_image']['id']==IMAGE and m['web']['archive_sha256']==WEB
    migration_path=find(a.migration,'MIGRATION-RESULT.json');migration=json.loads(migration_path.read_text())
    browser_path=find(a.browser,'BROWSER-ACCEPTANCE.json');browser=json.loads(browser_path.read_text())
    isolation_path=find(a.isolation,'ISOLATION-RESULT.json');isolation=json.loads(isolation_path.read_text())
    assert migration['status']=='passed' and migration['baseline_commit']=='4406320354e2241ca996a16ae97abb5dff4f477e'
    assert migration['candidate_core_image']['id']==IMAGE and migration['forward_migration_passed'] and migration['independent_schema14_restore_passed']
    assert browser['status']=='passed' and browser['core_image']['id']==IMAGE and browser['web_archive_sha256']==WEB and browser['original_browser_assertions_modified'] is False
    proof=browser['browser_native_proof'];assert proof['chromiumSandbox'] and proof['ignoreHTTPSErrors'] is False and proof['host_policy_changed'] is False and proof['host_trust_store_changed'] is False
    assert isolation['status']=='passed' and isolation['core_image']['id']==IMAGE
    a.out.mkdir(mode=0o700);evidence=a.out/'evidence';evidence.mkdir()
    selected=[f"lantai-{m['version']}-linux-amd64-image.tar.gz",'caddy-2.11.4-linux-amd64-image.tar.gz',m['web']['archive'],'LICENSE-LANTAI.txt','image-version.json','FROZEN-SOURCE-PROOF.json','first-start-lifecycle.json','first-start-image-smoke.txt','gateway-smoke.txt','web-build.txt','backup-preparation.json']
    for name in selected:shutil.copyfile(a.candidate/name,a.out/name)
    for original,name in [(migration_path,'RC6-MIGRATION-RESTORE.json'),(browser_path,'ACTUAL-BROWSER.json'),(isolation_path,'ISOLATION.json'),(find(a.browser,'NATIVE-CA-PROBE.json'),'NATIVE-CA-PROBE.json'),(a.proof,'DISTRIBUTION-SOURCE-PROOF.json'),(a.candidate/'delivery-image-smoke.json','INITIAL-ACCEPTANCE-FAILED.json'),(a.candidate/'INSTALL-PRIVACY-CHECK.json','INITIAL-INSTALL-PRIVACY-CHECK.json')]:shutil.copyfile(original,evidence/name)
    shutil.copyfile(ROOT/'packaging/r10/compose.gui.migrate.r1.yaml',a.out/'compose.r10.gui.migrate.r1.yaml')
    shutil.copyfile(ROOT/'packaging/r10/GUI-MIGRATION-r1.md',a.out/'GUI-MIGRATION-r1.md')
    # The original archive scans these exact unchanged image/Web bytes. New
    # selected text/JSON is checked separately. Never select profiles, snapshots,
    # raw enrollment, backups, keys, sessions or screenshots for the installer.
    sensitive={'password','passphrase','totp_secret','session_token','access_token','refresh_token','authorization','cookie','csrf_token','recovery_codes','private_key'}
    def inspect(v,label):
        if isinstance(v,dict):
            for k,x in v.items():
                assert not(k.lower() in sensitive and (isinstance(x,str) and x or isinstance(x,list) and any(isinstance(z,str) and z for z in x))),label+':'+k
                inspect(x,label)
        elif isinstance(v,list):
            for x in v:inspect(x,label)
    for p in a.out.rglob('*'):
        if not p.is_file() or p.name.endswith('.tar.gz'):continue
        raw=p.read_bytes();assert not re.search(rb'-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----',raw),p.name
        try:v=json.loads(raw)
        except (ValueError,UnicodeDecodeError):continue
        inspect(v,p.name)
    manifest={'schema':'lantai.r10-gui-migration-delivery/v1','distribution_revision':'r10-gui-migration-r1','status':'guarded-rc6-upgrade-and-browser-validated-on-synthetic-linux-amd64',
        'image_web_build_commit':SOURCE,'distribution_commit':os.environ['GITHUB_SHA'],'frozen_product_sha256':'dbca6589325d6e043f9c85b914912683ba3f99a0d9fa0d4f2d55ef85a5c6d09c','product_source_modified':False,'image_and_web_bytes_rebuilt':False,
        'core_image':m['core_image'],'caddy_image':m['caddy_image'],'web':m['web'],
        'initial_bundle':{'run':39535413064,'artifact_id':12317900785,'size_bytes':39574883,'sha256':'27b5f63ff361a25d793f4a85305ff5d3e4ef7bff2b5ecdf6bb4c7ec92456a090','old_gate_status':'failed; original record retained; this distribution uses the separately validated guarded migration configuration'},
        'validation':{'rc6_migration_restore_run':39538298792,'rc6_checks':len(migration['checks']),'browser_run':39541828961,'browser_checks':len(browser['checks']),'cold_start_and_same_volume_new_version_restart':True,'internal_network_probe':isolation,'guarded_14_to15_migration':True,'independent_schema14_restore_with_operator':True,'actual_browser_scenarios':browser['browser'],'certificate_setup':'Chrome native profile import; documented FileChooser protocol fixture in a private test-session; no host policy/trust change or certificate-validation override'},
        'boundaries':{'same_volume_old_image_downgrade_supported':False,'NAS_GUI_only_old_version_recovery_verified':False,'old_restore_operator_steps':['recover-admin','restore-complete with bound review/evidence'],'NAS_accessed_or_deployed':False,'real_DCC_editing_executed':False},
        'required_GUI_input':{'LANTAI_UPGRADE_BACKUP_DIR':'NAS host backup bind directory / completed job.id; must directly contain verified manifest.json. No real value prefilled.'},
        'privacy':{'unchanged_image_web_original_scan_passed':True,'new_selected_JSON_and_text_scan_passed':True,'test_secrets_keys_sessions_backup_packages_profiles_screenshots_in_installer':False},
        'publication':{'candidate_artifact_only':True,'main_merged':False,'formal_release':False,'container_registry_published':False}}
    manifest['files']=[{'path':str(p.relative_to(a.out)),'bytes':p.stat().st_size,'sha256':sha(p)} for p in sorted(a.out.rglob('*')) if p.is_file()]
    save(a.out/'manifest.json',manifest)
    (a.out/'SHA256SUMS').write_text(''.join(f'{sha(p)}  {p.relative_to(a.out)}\n' for p in sorted(a.out.rglob('*')) if p.is_file() and p.name!='SHA256SUMS'))
    print('FINAL_SEAL '+json.dumps({'distribution_revision':manifest['distribution_revision'],'core_image':manifest['core_image'],'web_archive_sha256':WEB,'files':[{'path':str(p.relative_to(a.out)),'bytes':p.stat().st_size,'sha256':sha(p)} for p in sorted(a.out.rglob('*')) if p.is_file()],'same_volume_downgrade_supported':False,'GUI_only_old_recovery_verified':False}),flush=True)
if __name__=='__main__':main()
