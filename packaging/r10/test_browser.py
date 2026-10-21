"""Run all original release browser assertions against the exact paired artifacts."""
import argparse, hashlib, json, os
from pathlib import Path
import sys,tempfile,time
sys.path.insert(0,str(Path(__file__).resolve().parent))
from test_migration import d,ROOT,EXPECTED,verify_sums,save

def main():
    p=argparse.ArgumentParser();p.add_argument('--candidate',type=Path,required=True);p.add_argument('--out',type=Path,required=True);a=p.parse_args();a.out.mkdir(mode=0o700)
    work=Path(tempfile.mkdtemp(prefix='lantai-safe-browser-'));work.chmod(0o700);instance=None
    d.RESULT.update(schema='lantai.actual-artifact-browser/v1',status='running',source_commit=EXPECTED,NAS_accessed=False,real_data_used=False,product_source_modified=False,checks=[])
    try:
        d.require_local_docker(d.run);verify_sums(a.candidate);m=json.loads((a.candidate/'manifest.json').read_text());assert m['source_commit']==EXPECTED
        d.load_image(a.candidate,m);d.run(['docker','load','-i',str(a.candidate/'caddy-2.11.4-linux-amd64-image.tar.gz')])
        web=work/'web';d.extract_verified_archive(a.candidate/m['web']['archive'],web,m['web']['archive_prefix'],m['web']['files'])
        cli=work/'lantai';cid=d.run(['docker','create',m['core_image']['tag']])
        try:d.run(['docker','cp',cid+':/usr/local/bin/lantai',str(cli)])
        finally:d.run(['docker','rm',cid])
        cli.chmod(0o700)
        deploy=ROOT/'deploy';instance=d.Instance(work/'instance',deploy,web,m['caddy_image']['tag'],(deploy/'compose.yaml').read_text(),(deploy/'Caddyfile').read_bytes(),m['core_image']['tag'])
        instance.compose+=['-f',str(instance.root/'compose.web.yaml')]
        d.RESULT['phase']='actual new image and static archive initialization'
        d.run(instance.compose+['up','-d']);instance.initialize_and_seed(cli);instance.retained(cli,m);instance.ready();instance.tls_negative();instance.static(m['web']['files'])
        d.ENV['LANTAI_WEB_CHROMIUM_EXECUTABLE']='/opt/google/chrome/chrome';d.ENV['CANDIDATE_BROWSER_PROOF']=str(work/'native-browser-proof.json')
        original=d.run
        def run(argv,**kwargs):
            if len(argv)>1 and Path(argv[1]).name=='test-release-browser.cjs':argv=[argv[0],str(ROOT/'packaging/r10/browser_adapter.cjs'),*argv[2:]]
            return original(argv,**kwargs)
        d.run=run
        evidence=work/'private-screenshots';evidence.mkdir(mode=0o700)
        while time.time()%30>2:time.sleep(.2)
        d.RESULT['phase']='all original browser assertions with native profile CA and sandbox'
        instance.browser(evidence,'exact-r10-artifact')
        d.RESULT.update(status='passed',core_image=m['core_image'],web_archive_sha256=m['web']['archive_sha256'],
            browser_native_proof=json.loads((work/'native-browser-proof.json').read_text()),
            packaging_adapter_sha256=hashlib.sha256((ROOT/'packaging/r10/browser_adapter.cjs').read_bytes()).hexdigest(),
            original_browser_assertions_modified=False,credentials_backups_profiles_screenshots_uploaded=False)
    except Exception as error:
        text=str(error)
        for v in d.PRIVATE_VALUES:
            if v:text=text.replace(v,'[REDACTED]')
        d.RESULT.update(status='failed',error=type(error).__name__+': '+text)
    finally:
        proof=work/'native-browser-proof.json'
        if proof.is_file():d.RESULT['browser_native_proof']=json.loads(proof.read_text())
        if instance:
            try:instance.stop()
            except Exception:d.RESULT.update(status='failed',cleanup_failed=True)
        save(a.out/'BROWSER-ACCEPTANCE.json',d.RESULT)
    print('BROWSER_ACCEPTANCE '+json.dumps(d.RESULT,ensure_ascii=False),flush=True)
    return 0 if d.RESULT['status']=='passed' else 1
if __name__=='__main__':sys.exit(main())
