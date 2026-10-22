"""Load the existing r10 images, test the new Web, then switch only Web back."""
import argparse,hashlib,json,shutil,sys,tempfile
from pathlib import Path
ROOT=Path(__file__).resolve().parents[2];sys.path.insert(0,str(ROOT/'packaging/r10'))
from test_migration import d,EXPECTED,verify_sums,save

def main():
    ap=argparse.ArgumentParser();ap.add_argument('--baseline',type=Path,required=True);ap.add_argument('--web-build',type=Path,required=True);ap.add_argument('--out',type=Path,required=True);a=ap.parse_args();a.out.mkdir(mode=0o700)
    work=Path(tempfile.mkdtemp(prefix='lantai-safe-browser-r11-web-'));work.chmod(0o700);instance=None
    d.RESULT.update(schema='lantai.r11-web-compatible-core-browser/v1',status='running',revision='r11-web-help',NAS_accessed=False,real_data_used=False,core_rebuilt=False,schema_changed=False,checks=[])
    try:
        d.require_local_docker(d.run);verify_sums(a.baseline);m=json.loads((a.baseline/'manifest.json').read_text());assert m['source_commit']==EXPECTED
        new=json.loads((a.web_build/'web-manifest.json').read_text());assert d.sha256(a.web_build/new['archive'])==new['archive_sha256']
        d.load_image(a.baseline,m);d.run(['docker','load','-i',str(a.baseline/'caddy-2.11.4-linux-amd64-image.tar.gz')])
        web,old=work/'new-web',work/'rollback-web'
        for directory,meta,target in [(a.web_build,new,web),(a.baseline,m['web'],old)]:d.extract_verified_archive(directory/meta['archive'],target,meta['archive_prefix'],meta['files'])
        cli=work/'lantai';container=d.run(['docker','create',m['core_image']['tag']])
        try:d.run(['docker','cp',container+':/usr/local/bin/lantai',str(cli)])
        finally:d.run(['docker','rm',container])
        cli.chmod(0o700);deploy=ROOT/'deploy'
        instance=d.Instance(work/'instance',deploy,web,m['caddy_image']['tag'],(deploy/'compose.yaml').read_text(),(deploy/'Caddyfile').read_bytes(),m['core_image']['tag']);instance.compose+=['-f',str(instance.root/'compose.web.yaml')]
        d.RESULT['phase']='unchanged core with new static archive'
        d.run(instance.compose+['up','-d']);instance.initialize_and_seed(cli);instance.ready();instance.retained(cli,m);instance.static(new['files']);core_id=instance.cid()
        assert d.inspect(core_id)['Image']==m['core_image']['id'];instance.tls_negative()
        shots=work/'screenshots';shots.mkdir(mode=0o700)
        def browser(mode):
            proof=work/(mode+'-native-proof.json');d.ENV['CANDIDATE_BROWSER_PROOF']=str(proof);d.ENV['CANDIDATE_SYNTHETIC_CA']=str(instance.tls/'cert.pem')
            payload={'mode':mode,'origin':instance.origin,'ca_file':str(instance.tls/'cert.pem'),'browser_profile':str(work/(mode+'-private-profile')),'name':instance.name,'password':instance.password,'factor_hex':instance.factor.hex(),'receipt':instance.receipt,'principal_id':instance.who['principal']['principal_id'],'screenshots':str(shots),'source_sha256':hashlib.sha256(instance.content).hexdigest()}
            response=json.loads(d.run(['node',str(ROOT/'packaging/r11/browser.cjs')],stdin=json.dumps(payload)));assert response['passed'];return {'result':response,'native_proof':json.loads(proof.read_text())}
        d.RESULT['phase']='new Web navigation and manual browser checks';d.RESULT['new_web_browser']=browser('new-web')
        d.RESULT['phase']='same-core Web-only rollback';instance.web=old;instance.select(m['core_image']['tag']);d.run(instance.compose+['up','-d','--no-deps','gateway']);assert instance.cid()==core_id
        instance.ready();instance.retained(cli,m);instance.static(m['web']['files']);d.RESULT['rollback_browser']=browser('rollback')
        d.RESULT.update(status='passed',source_commit=new['source_commit'],core_image=m['core_image'],new_web_sha256=new['archive_sha256'],rollback_web_sha256=m['web']['archive_sha256'],core_container_unchanged_during_Web_switch=True,original_instance_session_and_file_bytes_retained=True,credentials_profiles_or_backups_uploaded=False)
    except Exception as error:
        text=str(error)
        for secret in d.PRIVATE_VALUES:
            if secret:text=text.replace(secret,'[REDACTED]')
        d.RESULT.update(status='failed',error=type(error).__name__+': '+text)
        try:d.RESULT['browser_diagnostic']=json.loads(d.RESULT.get('command_stderr_redacted',''))
        except ValueError:pass
    finally:
        images=[]
        for name in ['r11-desktop-navigation.png','r11-help-834.png','r11-help-390.png']:
            p=work/'screenshots'/name
            if p.is_file():shutil.copyfile(p,a.out/name);images.append({'name':name,'sha256':d.sha256(p),'synthetic_post_login_or_public_help_only':True})
        d.RESULT['screenshots']=images
        if instance:
            try:instance.stop()
            except Exception:d.RESULT.update(status='failed',cleanup_failed=True)
        save(a.out/'WEB-BROWSER-RESULT.json',d.RESULT)
    print('WEB_BROWSER_RESULT '+json.dumps(d.RESULT,ensure_ascii=False),flush=True);return 0 if d.RESULT['status']=='passed' else 1
if __name__=='__main__':sys.exit(main())
