"""Prove this is a new Web/docs revision with the frozen r10 backend retained."""
import argparse,hashlib,json,stat,subprocess
from pathlib import Path
ROOT=Path(__file__).resolve().parents[2]
BASE='9e5d164427b2fb5da5147de9180dad2eb68228f1'
R10='dbca6589325d6e043f9c85b914912683ba3f99a0d9fa0d4f2d55ef85a5c6d09c'
NEW_PRODUCT={'docs/user-guide/acceptance.md','web/public/help/acceptance.html'}
def allowed(path):return path in {'README.md','README.en.md','web/src/main.tsx'} or path.startswith(('docs/user-guide/','web/public/help/'))
def sha(path):return hashlib.sha256(path.read_bytes()).hexdigest()
def main():
    ap=argparse.ArgumentParser();ap.add_argument('--out',type=Path,required=True);a=ap.parse_args()
    frozen=json.loads((ROOT/'packaging/r10/FROZEN-SOURCE.json').read_text());assert frozen['tree_sha256']==R10
    changed=[];rows=[];unchanged=0
    for old in frozen['files']:
        p=ROOT/old['path'];assert p.is_file() and not p.is_symlink()
        same=sha(p)==old['sha256'] and p.stat().st_size==old['bytes'] and bool(p.stat().st_mode&stat.S_IXUSR)==bool(old['mode']&stat.S_IXUSR)
        if not same:assert allowed(old['path']),old['path'];changed.append(old['path'])
        elif not allowed(old['path']):unchanged+=1
        rows.append({'path':old['path'],'git_mode':'100755' if p.stat().st_mode&stat.S_IXUSR else '100644','bytes':p.stat().st_size,'sha256':sha(p)})
    for name in sorted(NEW_PRODUCT):
        p=ROOT/name;assert p.is_file() and not p.is_symlink();rows.append({'path':name,'git_mode':'100644','bytes':p.stat().st_size,'sha256':sha(p)});changed.append(name)
    # A source-only allowlist also prevents incidental core/Compose/dependency changes.
    diff=subprocess.check_output(['git','diff','--name-only',BASE,'HEAD'],cwd=ROOT,text=True).splitlines()
    assert all(allowed(n) or n.startswith('packaging/r11/') or n=='.github/workflows/r11-web-help.yml' for n in diff),diff
    old_main=subprocess.check_output(['git','show',BASE+':web/src/main.tsx'],cwd=ROOT,text=True)
    before='  const nav = [\n';first='    ["/", "home", "首页"],\n    ["/projects", "folder", "我的项目"],\n'
    assert old_main.count(first)==1;expected=old_main.replace(first,'').replace(before,before+first)
    assert (ROOT/'web/src/main.tsx').read_text()==expected,'unexpected application code change outside navigation order'
    rows.sort(key=lambda x:x['path']);digest=hashlib.sha256(json.dumps(rows,ensure_ascii=False,sort_keys=True,separators=(',',':')).encode()).hexdigest()
    result={'schema':'lantai.web-only-revision-proof/v1','revision':'r11-web-help','source_commit':subprocess.check_output(['git','rev-parse','HEAD'],cwd=ROOT,text=True).strip(),'base_commit':BASE,'legacy_r10_frozen_tree_sha256':R10,'canonical_product_sha256':digest,'canonical_product_files':len(rows),'canonical_algorithm':'sorted path/git_mode/bytes/sha256 JSON, UTF-8, sorted keys, compact separators','all_backend_product_files_unchanged':True,'unchanged_non_web_document_product_files':unchanged,'core_rebuilt':False,'schema_changed':False,'application_code_change':'navigation order only','changed_product_files':sorted(changed)}
    a.out.write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n');print(json.dumps(result,ensure_ascii=False))
if __name__=='__main__':main()
