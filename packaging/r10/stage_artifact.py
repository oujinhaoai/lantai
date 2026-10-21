"""Select install artifacts only; never upload source fixtures or test runtime."""
import argparse, gzip, hashlib, io, json, re, shutil, tarfile
from pathlib import Path

ROOT=Path(__file__).resolve().parents[2]
def digest(p):
    with p.open('rb') as f:return hashlib.file_digest(f,'sha256').hexdigest()
def save(p,x):p.write_text(json.dumps(x,ensure_ascii=False,indent=2)+'\n')
def main():
    ap=argparse.ArgumentParser();ap.add_argument('--build',type=Path,required=True);ap.add_argument('--proof',type=Path,required=True);ap.add_argument('--out',type=Path,required=True);a=ap.parse_args()
    m=json.loads((a.build/'manifest.json').read_text());proof=json.loads(a.proof.read_text())
    assert m['source_commit']==proof['source_commit']
    assert proof['canonical_frozen_tree_sha256']=='dbca6589325d6e043f9c85b914912683ba3f99a0d9fa0d4f2d55ef85a5c6d09c'
    acceptance_failed=m.get('status')=='built-with-acceptance-failure'
    assert (m['delivery_smoke']['status']=='passed' or
            acceptance_failed and m['delivery_smoke']['status']=='failed')
    assert m['first_start_smoke']['status']=='passed'
    a.out.mkdir(mode=0o700)
    selected=[f"lantai-{m['version']}-linux-amd64-image.tar.gz",'caddy-2.11.4-linux-amd64-image.tar.gz',m['web']['archive']]
    for n in selected:
        assert '/' not in n and (a.build/n).is_file()
        shutil.copyfile(a.build/n,a.out/n)
    for n in ['image-version.json','first-start-lifecycle.json','first-start-image-smoke.txt','delivery-image-smoke.json','delivery-image-smoke.txt','backup-preparation.json','gateway-smoke.txt','web-build.txt','browser-sandbox-preflight.json']:
        assert (a.build/n).is_file(),n
        shutil.copyfile(a.build/n,a.out/n)
    shutil.copyfile(a.proof,a.out/'FROZEN-SOURCE-PROOF.json')
    shutil.copyfile(ROOT/'LICENSE',a.out/'LICENSE-LANTAI.txt')
    compose=(ROOT/'packaging/r10/compose.gui.template.yaml').read_text().replace('__CORE_IMAGE__',m['core_image']['tag'])
    (a.out/'compose.r10.gui.public-template.yaml').write_text(compose)
    (a.out/'GUI-UPGRADE.md').write_text((ROOT/'packaging/r10/GUI-UPGRADE.md').read_text().replace('__CORE_IMAGE__',m['core_image']['tag']))
    private_key=re.compile(rb'-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----')
    findings=[];scanned=[]
    def scan(label,data):
        if private_key.search(data):findings.append({'path':label,'kind':'private_key_header'})
        try:value=json.loads(data)
        except (ValueError,UnicodeDecodeError):value=None
        credential_fields={'password','passphrase','session_token','access_token','refresh_token','totp_secret','private_key','recovery_codes','authorization','cookie','csrf_token'}
        def inspect_json(item):
            if isinstance(item,dict):
                for key,child in item.items():
                    if key.lower() in credential_fields and (isinstance(child,str) and child or isinstance(child,list) and any(isinstance(x,str) and x for x in child)):
                        findings.append({'path':label,'kind':'credential_value_in_json','field':key})
                    inspect_json(child)
            elif isinstance(item,list):
                for child in item:inspect_json(child)
        inspect_json(value)
        if b'\0' not in data[:8192]:
            for pattern,kind in [(rb'(?i)(?:password|passphrase)\s*[:=]\s*[\x22\x27][^\x22\x27\n]{3,}','password_literal'),(rb'https://[^\s\x22\x27<>]+[?&](?:sig|access_token)=','signed_url')]:
                if re.search(pattern,data):findings.append({'path':label,'kind':kind})
        scanned.append({'path':label,'bytes':len(data),'sha256':hashlib.sha256(data).hexdigest()})
    def archive(label,data,depth=0):
        assert depth<6
        with tarfile.open(fileobj=io.BytesIO(data),mode='r:*') as t:
            for member in t:
                assert not member.name.startswith('/') and '..' not in Path(member.name).parts
                if not member.isfile():continue
                raw=t.extractfile(member).read();name=label+'!/'+member.name
                if raw[257:262]==b'ustar' or raw[:2]==b'\x1f\x8b':archive(name,raw,depth+1)
                else:scan(name,raw)
    for p in sorted(a.out.iterdir()):
        if p.name.endswith('.tar.gz'):archive(p.name,p.read_bytes())
        else:scan(p.name,p.read_bytes())
    assert not findings,json.dumps(findings)
    save(a.out/'INSTALL-PRIVACY-CHECK.json',{'status':'passed','files_checked':len(scanned),'findings':findings,'source_tests_and_test_runtime_excluded':True,'test_keys_sessions_backup_packages_screenshots_uploaded':False,'files':scanned})
    records=[{'name':p.name,'size_bytes':p.stat().st_size,'sha256':digest(p)} for p in sorted(a.out.iterdir())]
    result={'schema':'lantai.r10-github-candidate/v1','status':'candidate-built-and-synthetic-smoke-passed','source_commit':m['source_commit'],'frozen_source':proof,'version':m['version'],'core_image':m['core_image'],'caddy_image':m['caddy_image'],'web':m['web'],'files':records,'delivery_smoke':m['delivery_smoke'],'first_start_smoke':m['first_start_smoke'],'original_cloud_image_id':'sha256:7e147ab1daf21b29090f46425c1e7feba0868e86af5afc21a0efdfd68f078dfe','new_build_image_id_must_be_used':True,'toolchain':m['go_version'],'baseline_commit':m['delivery_smoke']['baseline_commit'],'actual_user_rc6_upgrade_or_rollback_verified':False,'NAS_deployed':False,'DCC_editing_executed':False,'formal_release':False,'container_registry_published':False}
    if acceptance_failed:
        result['status']='candidate-built-acceptance-failed'
        result['fully_accepted']=False
    else:
        result['fully_accepted']=True
    result['source_url']='https://github.com/oujinhaoai/lantai/tree/'+m['source_commit']
    save(a.out/'manifest.json',result)
    (a.out/'SHA256SUMS').write_text(''.join(f'{digest(p)}  {p.name}\n' for p in sorted(a.out.iterdir()) if p.name!='SHA256SUMS'))
    print(json.dumps({'source_commit':result['source_commit'],'core_image':result['core_image'],'web':result['web'],'artifact_files':[{'name':p.name,'bytes':p.stat().st_size,'sha256':digest(p)} for p in sorted(a.out.iterdir())],'privacy':'passed','actual_user_rc6_upgrade_or_rollback_verified':False}))
if __name__=='__main__':main()
