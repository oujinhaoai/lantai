"""Build only the Web using the existing release bundle implementation."""
import argparse,json,subprocess,sys
from pathlib import Path
ROOT=Path(__file__).resolve().parents[2];sys.path.insert(0,str(ROOT/'scripts'))
from release_bundle import build_web_bundle

def run(argv,**kwargs):
    p=subprocess.run(argv,text=True,capture_output=True,check=False,**kwargs)
    if p.returncode:raise RuntimeError('Web build check failed: '+' '.join(argv)+'\n'+p.stdout[-3500:]+'\n'+p.stderr[-3500:])
    return p.stdout.strip()
def main():
    ap=argparse.ArgumentParser();ap.add_argument('--out',type=Path,required=True);a=ap.parse_args();a.out.mkdir(mode=0o700)
    commit=run(['git','rev-parse','HEAD'],cwd=ROOT);stamp=int(run(['git','show','-s','--format=%ct','HEAD'],cwd=ROOT))
    unit=run(['npm','run','test:unit','--prefix','web'],cwd=ROOT)
    (a.out/'web-unit-tests.txt').write_text(unit+'\n')
    web=build_web_bundle(ROOT,a.out,'v0.1.0-r11-web-help',commit,stamp,run)
    assert any(x['path']=='help/acceptance.html' for x in web['files'])
    (a.out/'web-manifest.json').write_text(json.dumps(web,ensure_ascii=False,indent=2)+'\n');print('WEB_BUILD '+json.dumps(web,ensure_ascii=False))
if __name__=='__main__':main()
