"""Additional business UI smoke against immutable exact image/Web artifacts."""
import argparse,hashlib,json,sys,tempfile
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parent))
from test_migration import d,ROOT,EXPECTED,verify_sums,save

def main():
    ap=argparse.ArgumentParser();ap.add_argument('--candidate',type=Path,required=True);ap.add_argument('--out',type=Path,required=True);a=ap.parse_args();a.out.mkdir(mode=0o700)
    # Reuse the pinned safe-browser test namespace accepted by the certificate
    # picker. The picker's path guard remains unchanged.
    work=Path(tempfile.mkdtemp(prefix='lantai-safe-browser-five-entry-'));work.chmod(0o700);instance=None
    d.RESULT.update(schema='lantai.actual-five-entry-browser/v1',status='running',source_commit=EXPECTED,NAS_accessed=False,real_data_used=False,product_source_modified=False,checks=[])
    try:
        d.require_local_docker(d.run);verify_sums(a.candidate);m=json.loads((a.candidate/'manifest.json').read_text());assert m['source_commit']==EXPECTED
        d.load_image(a.candidate,m);d.run(['docker','load','-i',str(a.candidate/'caddy-2.11.4-linux-amd64-image.tar.gz')])
        web=work/'web';d.extract_verified_archive(a.candidate/m['web']['archive'],web,m['web']['archive_prefix'],m['web']['files'])
        cli=work/'lantai';container=d.run(['docker','create',m['core_image']['tag']])
        try:d.run(['docker','cp',container+':/usr/local/bin/lantai',str(cli)])
        finally:d.run(['docker','rm',container])
        cli.chmod(0o700);deploy=ROOT/'deploy'
        instance=d.Instance(work/'instance',deploy,web,m['caddy_image']['tag'],(deploy/'compose.yaml').read_text(),(deploy/'Caddyfile').read_bytes(),m['core_image']['tag'])
        instance.compose+=['-f',str(instance.root/'compose.web.yaml')];instance.enable_backup(deploy);instance.select(m['core_image']['tag'])
        d.RESULT['phase']='private synthetic initialized core and backup volume'
        d.run(instance.compose+['up','-d']);instance.initialize_and_seed(cli);instance.ready()
        d.ENV['CANDIDATE_BROWSER_PROOF']=str(work/'native-proof.json');d.ENV['CANDIDATE_SYNTHETIC_CA']=str(instance.tls/'cert.pem')
        private_downloads=work/'private-downloads';private_downloads.mkdir(mode=0o700)
        payload={'origin':instance.origin,'ca_file':str(instance.tls/'cert.pem'),'browser_profile':str(work/'private-profile'),'name':instance.name,'password':instance.password,'factor_hex':instance.factor.hex(),'receipt':instance.receipt,'principal_id':instance.who['principal']['principal_id'],'private_downloads':str(private_downloads),'source_bytes_sha256':hashlib.sha256(instance.content).hexdigest()}
        d.RESULT['phase']='additional five actual browser business flows'
        response=json.loads(d.run(['node',str(ROOT/'packaging/r10/five_entry_browser.cjs')],stdin=json.dumps(payload)))
        assert response['same_principal'] and response['cookie_protected'] and response['all_five_entries_exercised']
        d.RESULT.update(status='passed' if response['passed'] else 'failed',business_browser=response,core_image=m['core_image'],web_archive_sha256=m['web']['archive_sha256'],credentials_profiles_downloads_backup_packages_uploaded=False)
    except Exception as error:
        value=str(error)
        for private in d.PRIVATE_VALUES:
            if private:value=value.replace(private,'[REDACTED]')
        d.RESULT.update(status='failed',error=type(error).__name__+': '+value)
        try:
            diagnostic=json.loads(d.RESULT.get('command_stderr_redacted',''))
            d.RESULT['completed_business_checks']=diagnostic.get('completed_checks',[])
            d.RESULT['safe_DOM']=diagnostic.get('safe_DOM')
        except ValueError:pass
    finally:
        proof=work/'native-proof.json'
        if proof.is_file():d.RESULT['browser_native_proof']=json.loads(proof.read_text())
        if instance:
            try:instance.stop()
            except Exception:d.RESULT.update(status='failed',cleanup_failed=True)
        save(a.out/'FIVE-ENTRY-ACCEPTANCE.json',d.RESULT)
    print('FIVE_ENTRY_ACCEPTANCE '+json.dumps(d.RESULT,ensure_ascii=False),flush=True)
    return 0 if d.RESULT['status']=='passed' else 1
if __name__=='__main__':sys.exit(main())
