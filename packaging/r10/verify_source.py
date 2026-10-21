"""Verify the frozen r10 product bytes; Git stores only executable mode bits."""
import argparse, hashlib, json, stat, subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
EXPECTED = 'dbca6589325d6e043f9c85b914912683ba3f99a0d9fa0d4f2d55ef85a5c6d09c'
EXTRAS = {'.github/workflows/candidate-r10.yml', 'packaging/r10/FROZEN-SOURCE.json',
          'packaging/r10/verify_source.py', 'packaging/r10/stage_artifact.py',
          'packaging/r10/compose.gui.template.yaml', 'packaging/r10/GUI-UPGRADE.md',
          'packaging/r10/build_candidate.py', '.github/workflows/r10-migration.yml',
          'packaging/r10/test_migration.py', '.github/workflows/r10-browser.yml',
          'packaging/r10/probe_browser.cjs', 'packaging/r10/browser_adapter.cjs',
          'packaging/r10/test_browser.py', 'packaging/r10/native_file_dialog.py',
          'packaging/r10/probe_native_ca.cjs', 'packaging/r10/ca_file_portal.py'}

def verify():
    frozen = json.loads((ROOT/'packaging/r10/FROZEN-SOURCE.json').read_text())
    actual = []
    for row in frozen['files']:
        path = ROOT/row['path']
        assert path.is_file() and not path.is_symlink(), row['path']
        raw = path.read_bytes()
        assert len(raw) == row['bytes'] and hashlib.sha256(raw).hexdigest() == row['sha256'], row['path']
        assert bool(path.stat().st_mode & stat.S_IXUSR) == bool(row['mode'] & stat.S_IXUSR), row['path']
        actual.append(dict(row))
    digest = hashlib.sha256(json.dumps(actual, ensure_ascii=False, sort_keys=True, separators=(',',':')).encode()).hexdigest()
    assert digest == frozen['tree_sha256'] == EXPECTED
    tracked = set(subprocess.check_output(['git','ls-files','-z'],cwd=ROOT).decode().split('\0'))-{''}
    assert tracked == {x['path'] for x in actual} | EXTRAS, sorted(tracked ^ ({x['path'] for x in actual}|EXTRAS))
    commit = subprocess.check_output(['git','rev-parse','HEAD'],cwd=ROOT,text=True).strip()
    return {'schema':'lantai.frozen-source-proof/v1','source_commit':commit,'frozen_product_files':len(actual),
            'canonical_frozen_tree_sha256':digest,'all_product_bytes_and_executable_bits_match':True,
            'original_nonexecutable_mode': '0600; Git checkout normalizes to 0644 without changing contents',
            'packaging_overlay_files':sorted(EXTRAS),'original_source_base_commit':frozen['source_base_commit']}

if __name__ == '__main__':
    p=argparse.ArgumentParser();p.add_argument('--out',type=Path,required=True);a=p.parse_args()
    result=verify();a.out.write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n');print(json.dumps(result))
