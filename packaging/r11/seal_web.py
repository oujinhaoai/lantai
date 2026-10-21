"""Stage only verified static archives, public instructions and a scoped receipt."""
import argparse,hashlib,json,re,shutil,sys,tarfile
from pathlib import Path
ROOT=Path(__file__).resolve().parents[2];sys.path.insert(0,str(ROOT/'scripts'))
from release_bundle import sha256
CORE={'tag':'lantai:v0.1.0-r10-candidate-3ad86ea10830','id':'sha256:38d2114be464e0ba333f2bf4b0a3e88cfbd41bed5b474728e2513df644034945','source_commit':'3ad86ea10830676d5ef5d72286a02fd0d9d01c65'}
def main():
    ap=argparse.ArgumentParser()
    for key in ['build','baseline','proof','evidence','out']:ap.add_argument('--'+key,type=Path,required=True)
    a=ap.parse_args();new=json.loads((a.build/'web-manifest.json').read_text());proof=json.loads(a.proof.read_text());old=json.loads((a.baseline/'manifest.json').read_text());assert new['source_commit']==proof['source_commit'] and proof['all_backend_product_files_unchanged']
    assert old['core_image']['id']==CORE['id'] and old['web']['archive_sha256']=='0c21322ab7506562dc0dc32e53c63b51aef6fd48184a93127ca9f3fd40707dfd'
    assert sha256(a.build/new['archive'])==new['archive_sha256'] and sha256(a.baseline/old['web']['archive'])==old['web']['archive_sha256']
    result_path=a.evidence/'WEB-BROWSER-RESULT.json';result=json.loads(result_path.read_text()) if result_path.is_file() else {'status':'unexecuted'}
    passed=result['status']=='passed';a.out.mkdir(mode=0o700);(a.out/'rollback').mkdir()
    shutil.copyfile(a.build/new['archive'],a.out/new['archive']);shutil.copyfile(a.baseline/old['web']['archive'],a.out/'rollback'/old['web']['archive'])
    for source,name in [(a.proof,'source-proof.json'),(ROOT/'packaging/r11/WEB-UPDATE.md','WEB-UPDATE.md'),(ROOT/'LICENSE','LICENSE-LANTAI.txt'),(a.build/'web-build.txt','web-build.txt'),(a.build/'web-unit-tests.txt','web-unit-tests.txt')]:shutil.copyfile(source,a.out/name)
    summary={'status':result['status'],'core_image':result.get('core_image',CORE),'core_container_unchanged_during_Web_switch':result.get('core_container_unchanged_during_Web_switch',False),'original_instance_session_and_file_bytes_retained':result.get('original_instance_session_and_file_bytes_retained',False),'new_web_checks':result.get('new_web_browser',{}).get('result',{}).get('checks',[]),'rollback_checks':result.get('rollback_browser',{}).get('result',{}).get('checks',[]),'screenshots_in_installer':False,'credentials_profiles_backups_in_installer':False}
    (a.out/'browser-check.json').write_text(json.dumps(summary,ensure_ascii=False,indent=2)+'\n')
    scanned=[];sensitive={'password','passphrase','totp_secret','session_token','access_token','refresh_token','private_key','recovery_codes','authorization','cookie','csrf_token'}
    def scan(label,body):
        assert not re.search(rb'-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----',body),label
        assert not re.search(rb'https://[^\s\x22\x27<>]+[?&](?:sig|access_token)=',body),label
        try:value=json.loads(body)
        except (ValueError,UnicodeDecodeError):value=None
        def inspect(v):
            if isinstance(v,dict):
                for key,x in v.items():
                    assert not(key.lower() in sensitive and (isinstance(x,str) and x or isinstance(x,list) and any(isinstance(y,str) and y for y in x))),label+':'+key
                    inspect(x)
            elif isinstance(v,list):
                for x in v:inspect(x)
        inspect(value);scanned.append({'path':label,'bytes':len(body),'sha256':hashlib.sha256(body).hexdigest()})
    for p in a.out.rglob('*'):
        if not p.is_file():continue
        if p.name.endswith('.tar.gz'):
            with tarfile.open(p,'r:gz') as archive:
                for member in archive:
                    assert member.isfile() and member.name.startswith('web/') and '..' not in Path(member.name).parts
                    scan(p.name+':'+member.name,archive.extractfile(member).read())
        else:scan(str(p.relative_to(a.out)),p.read_bytes())
    (a.out/'privacy-check.json').write_text(json.dumps({'status':'passed','scanned':scanned,'only_static_archives_and_public_selected_text':True},ensure_ascii=False,indent=2)+'\n')
    m={'schema':'lantai.web-update-delivery/v1','revision':'r11-web-help','status':'Web-built-and-browser-validated' if passed else 'Web-built-with-browser-validation-incomplete','source_commit':new['source_commit'],'base_commit':proof['base_commit'],'canonical_product_sha256':proof['canonical_product_sha256'],'product_revision_differs_from_r10':True,'original_r10_archives_modified':False,'all_backend_product_files_unchanged':True,'core_rebuilt':False,'schema_changed':False,'compatible_core':CORE,'web':new,'rollback_web':{'archive':'rollback/'+old['web']['archive'],'archive_sha256':old['web']['archive_sha256'],'archive_prefix':'web','files':old['web']['files']},'navigation_order':['首页','我的项目','素材库','迁移中心','工程工作集','备份与恢复','系统管理','使用帮助'],'admin_entries_remain_permission_filtered':True,'manual_pages':11,'validation':summary,'limitations':['NAS deployment and private paths not accessed','old-core same-volume schema downgrade unsupported','NAS GUI-only old-version recovery remains unverified','not a full five-entry or real DCC business acceptance'],'publication':{'candidate_artifact_only':True,'main_merged':False,'formal_release':False,'registry_published':False}}
    m['files']=[{'path':str(p.relative_to(a.out)),'bytes':p.stat().st_size,'sha256':sha256(p)} for p in sorted(a.out.rglob('*')) if p.is_file()]
    (a.out/'manifest.json').write_text(json.dumps(m,ensure_ascii=False,indent=2)+'\n');(a.out/'SHA256SUMS').write_text(''.join(sha256(p)+'  '+str(p.relative_to(a.out))+'\n' for p in sorted(a.out.rglob('*')) if p.is_file() and p.name!='SHA256SUMS'))
    print('WEB_DELIVERY '+json.dumps({'source_commit':m['source_commit'],'status':m['status'],'canonical_product_sha256':m['canonical_product_sha256'],'compatible_core':CORE,'web_archive_sha256':new['archive_sha256'],'files':[{'path':str(p.relative_to(a.out)),'bytes':p.stat().st_size,'sha256':sha256(p)} for p in sorted(a.out.rglob('*')) if p.is_file()]},ensure_ascii=False),flush=True)
if __name__=='__main__':main()
