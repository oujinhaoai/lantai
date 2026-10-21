#!/usr/bin/env python3
"""Edit/re-execute a pulled synthetic no-model recipe using installed ComfyUI."""
import argparse
import asyncio
import importlib.util
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
sys.dont_write_bytecode = True

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--comfy-source', type=Path, required=True)
    p.add_argument('--workspace', type=Path, required=True)
    p.add_argument('--files', type=Path, required=True)
    p.add_argument('--output', type=Path, required=True)
    p.add_argument('--mode', choices=['edit', 'reopen'], required=True)
    a = p.parse_args()
    source, workspace, output = a.comfy_source.resolve(), a.workspace.resolve(), a.output.resolve()
    if output.exists() or output.is_relative_to(workspace) or output.is_relative_to(source):
        p.error('new separate output directory required')
    output.mkdir(parents=True)
    asset = output / 'asset'
    files = json.loads(a.files.read_text())
    for item in files:
        name = item['path']
        old, new = (workspace / name).resolve(), (asset / name).resolve()
        if not old.is_relative_to(workspace) or not new.is_relative_to(asset):
            p.error('file path escapes synthetic asset')
        new.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(old, new)
    api = json.loads((asset / 'workflow-api.json').read_text())
    ui = json.loads((asset / 'workflow-ui.json').read_text())
    if {n['class_type'] for n in api.values()} != {'LoadImage', 'ImageScale', 'SaveImage'}:
        p.error('only synthetic LoadImage, ImageScale and SaveImage are allowed')
    if a.mode == 'edit':
        api['2']['inputs']['width'] += 64
        api['2']['inputs']['height'] += 32
        api['3']['inputs']['filename_prefix'] = 'post-recovery'
        for node in ui['nodes']:
            if str(node['id']) == '2':
                node['widgets_values'][1:3] = [api['2']['inputs']['width'], api['2']['inputs']['height']]
            if str(node['id']) == '3':
                node['widgets_values'][0] = 'post-recovery'
        (asset / 'workflow-api.json').write_text(json.dumps(api, indent=2)+'\n')
        (asset / 'workflow-ui.json').write_text(json.dumps(ui, indent=2)+'\n')
    runtime = output / 'runtime'
    for name in ['input','output','user','temp','models','cache']:
        (runtime/name).mkdir(parents=True)
    shutil.copyfile(asset/'input/synthetic.png', runtime/'input/synthetic.png')
    for key in ['TORCH_HOME','HF_HOME','XDG_CACHE_HOME','MPLCONFIGDIR','TRITON_CACHE_DIR','TORCHINDUCTOR_CACHE_DIR','NUMBA_CACHE_DIR']:
        os.environ[key]=str(runtime/'cache'/key.lower())
    os.environ.update(HF_HUB_OFFLINE='1',TRANSFORMERS_OFFLINE='1',HF_HUB_DISABLE_TELEMETRY='1',DO_NOT_TRACK='1',PYTHONDONTWRITEBYTECODE='1')
    tempfile.tempdir=str(runtime/'temp')
    spec=importlib.util.spec_from_file_location('synthetic_comfy',Path(__file__).with_name('comfy_native.py'))
    helper=importlib.util.module_from_spec(spec);spec.loader.exec_module(helper)
    sys.path.insert(0,str(source))
    sys.argv=['synthetic-comfy-continue','--base-directory',str(runtime),'--input-directory',str(runtime/'input'),'--output-directory',str(runtime/'output'),'--user-directory',str(runtime/'user'),'--temp-directory',str(runtime/'temp'),'--models-directory',str(runtime/'models'),'--cpu','--disable-dynamic-vram','--disable-all-custom-nodes','--disable-api-nodes','--disable-auto-launch','--cache-none','--database-url',f"sqlite:///{runtime/'user/comfyui.db'}"]
    os.chdir(output)
    policy=helper.audit_guard(source,workspace,output)
    report={'contract':'lantai.synthetic-comfy-continuation/v1','mode':a.mode,'network_listener':False,'models_loaded':False,'custom_nodes_loaded':False,'policy':policy}
    try:
        import comfy.options
        comfy.options.enable_args_parsing()
        import folder_paths
        import nodes
        import execution
        import comfyui_version
        import torch
        import numpy as np
        from PIL import Image
        if any(not Path(v).resolve().is_relative_to(output) for paths,_ in folder_paths.folder_names_and_paths.values() for v in paths):
            raise ValueError('runtime paths escaped output')
        valid=asyncio.run(execution.validate_prompt('post-recovery',api,None))
        if not valid[0] or valid[3]: raise ValueError(f'official validator rejected recipe: {valid}')
        executor=execution.PromptExecutor(helper.EventSink(),cache_type=execution.CacheType.NONE,cache_args={'ram':0,'ram_inactive':0})
        executor.execute(api,'post-recovery',extra_data={'extra_pnginfo':{'workflow':ui}},execute_outputs=valid[2])
        if not executor.success: raise ValueError(f'official executor failed: {executor.status_messages}')
        saved=list((runtime/'output').glob('*.png'))
        if len(saved)!=1: raise ValueError('exactly one output required')
        width,height=api['2']['inputs']['width'],api['2']['inputs']['height']
        with Image.open(saved[0]) as actual, Image.open(asset/'input/synthetic.png') as original:
            expected=np.asarray(original.convert('RGB').resize((width,height),Image.Resampling.NEAREST)).astype(np.int16)
            delta=int(np.abs(np.asarray(actual.convert('RGB')).astype(np.int16)-expected).max())
            if actual.size!=(width,height) or delta>1 or json.loads(actual.info['prompt'])!=api or json.loads(actual.info['workflow'])!=ui:
                raise ValueError('output pixels, dimensions or embedded recipe differ')
            if a.mode=='reopen':
                with Image.open(asset/'output/native.png') as stored:
                    if not np.array_equal(np.asarray(stored.convert('RGB')),np.asarray(actual.convert('RGB'))):
                        raise ValueError('reopened recipe did not reproduce committed pixels')
        report.update(status='passed',dimensions=[width,height],max_channel_delta=delta,comfyui_version=comfyui_version.__version__,torch_version=torch.__version__,python_version=sys.version.split()[0],validator_sha256=helper.digest(source/'execution.py'),nodes_sha256=helper.digest(source/'nodes.py'),status_messages=executor.status_messages)
        if a.mode=='edit':
            (asset/'output').mkdir(exist_ok=True)
            shutil.copyfile(saved[0],asset/'output/native.png')
            (asset/'run.json').write_text(json.dumps({'contract':'lantai.synthetic-native-run/v1','execution_status':'passed','recovered_recipe_modified':True,'dimensions':[width,height],'output':'output/native.png','output_sha256':helper.digest(saved[0]),'tool':report['comfyui_version']},indent=2)+'\n')
    except Exception as error:
        report.update(status='blocked',error=f'{type(error).__name__}: {error}')
        raise
    finally:
        (output/'validation.json').write_text(json.dumps(report,indent=2)+'\n')
    print(json.dumps({'status':report['status'],'mode':a.mode,'dimensions':report['dimensions']}))
if __name__=='__main__': main()
