#!/usr/bin/env python3
"""Migrate a stopped existing local Compose installation without copying keys.
No downloads or volume deletion. Failures report preparation/startup and cleanup state.
"""
import argparse
import os
import signal
import json
import re
import subprocess
import sys
from pathlib import Path

class Refused(RuntimeError):
    pass

def command(args):
    # A timeout must also terminate this invocation's Compose plugin children;
    # otherwise a surviving CLI child could keep starting services after cleanup.
    with subprocess.Popen(args,stdout=subprocess.PIPE,stderr=subprocess.PIPE,
                          text=True,start_new_session=True) as process:
        try:
            stdout,stderr=process.communicate(timeout=120)
        except (subprocess.TimeoutExpired,KeyboardInterrupt):
            try:
                os.killpg(process.pid,signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.communicate()
            raise
        if process.returncode:
            raise Refused(stderr.strip() or f"command failed ({process.returncode}): {args[0]}")
        return stdout

def persistent_source(config, mount):
    if mount['type'] == 'bind':
        return ('bind', str(Path(mount['source']).resolve()))
    if mount['type'] == 'volume':
        volume = config.get('volumes', {}).get(mount['source'], {})
        name = volume.get('name')
        if not name:
            raise Refused('resolved volume name is missing; refusing an ambiguous installation')
        return ('volume', name)
    raise Refused('persistent mount is not a bind or named volume')

def check_plan(config, old, image):
    """Pure read-only validation; every refusal happens before stopping services."""
    service = config['services']['core']
    if image.get('Os') != 'linux' or image.get('Architecture') != 'amd64':
        raise Refused('the reviewed local image must be native Linux amd64')
    if (service.get('user') != '65532:65532' or not service.get('read_only')
        or service.get('privileged') or 'ALL' not in service.get('cap_drop', [])
        or service.get('cap_add') or service.get('pull_policy') != 'never'):
        raise Refused('new core does not retain the default restricted service configuration')
    if service.get('networks') is None or set(service['networks']) != {'backend'} or not config['networks']['backend'].get('internal'):
        raise Refused('new core must use only the internal backend')
    if not any(s.replace(':','=') == 'no-new-privileges=true' for s in service.get('security_opt', [])):
        raise Refused('new core must retain no-new-privileges')
    mounts = {m['target']:m for m in service.get('volumes', [])}
    prior = {m['Destination']:m for m in old['Mounts']}
    for target in ['/var/lib/lantai', '/var/lib/lantai-secrets', '/var/lib/lantai-setup']:
        if target not in mounts or target not in prior:
            raise Refused(f'existing persistent mount missing: {target}')
        was = prior[target]
        source = (was['Type'], was.get('Name') if was['Type']=='volume' else str(Path(was['Source']).resolve()))
        if persistent_source(config, mounts[target]) != source:
            raise Refused(f'persistent source changed: {target}; reuse the existing project and environment')
    template = mounts.get('/etc/lantai/config.template.yaml')
    if not template or template['type'] != 'bind' or not template.get('read_only'):
        raise Refused('new deployment requires the immutable config input outside the data root')
    if '/var/lib/lantai/config.yaml' in mounts:
        raise Refused('new deployment still binds the live config read-only')
    legacy = prior.get('/var/lib/lantai/config.yaml')
    if legacy and (legacy['Type']!='bind' or legacy.get('RW') or str(Path(legacy['Source']).resolve())!=str(Path(template['source']).resolve())):
        raise Refused('new template is not the same effective read-only legacy config bind; correct LANTAI_CONFIG_FILE')
    return {'layout':'legacy_readonly_bind' if legacy else 'owned_config',
            'persistent_sources_unchanged':True, 'key_copy_or_rotation':False,
            'new_image_id':image['Id']}

def project_states(project, run=command):
    ids=run(['docker','ps','--all','--quiet','--filter','label=com.docker.compose.project='+project]).split()
    if not ids:
        return []
    records=json.loads(run(['docker','inspect',*ids]))
    states=[]
    for record in records:
        labels=record['Config'].get('Labels',{})
        service=labels.get('com.docker.compose.service')
        if labels.get('com.docker.compose.project')!=project or service not in ('core','gateway'):
            continue
        state=record.get('State',{})
        if not isinstance(state.get('Running'),bool) or state.get('Status') not in ('created','running','paused','restarting','removing','exited','dead'):
            raise Refused('container state is incomplete; shutdown cannot be confirmed')
        states.append({'container':record['Id'], 'service':service,
                       'oneoff':labels.get('com.docker.compose.oneoff','false').lower()=='true',
                       'running':bool(state.get('Running') or state.get('Restarting') or state.get('Paused')),
                       'status':state.get('Status','unknown')})
    return states

def fail_upgrade(phase, cause, compose, project, run, stop_after_failure):
    # Cleanup targets exactly this project's two application services. Never
    # down, remove volumes, stop other services, or touch another project.
    cleanup={'attempted':stop_after_failure, 'stop_succeeded':None}
    if stop_after_failure:
        try:
            run([*compose,'stop','core','gateway'])
            cleanup['stop_succeeded']=True
        except (Refused,subprocess.TimeoutExpired,OSError,KeyboardInterrupt) as exc:
            cleanup.update(stop_succeeded=False,stop_error=str(exc) or type(exc).__name__)
    try:
        states=project_states(project,run)
        cleanup['observed_states']=states
        cleanup['core_gateway_confirmed_stopped']=all(not s['running'] and s['status'] in ('created','exited','dead') for s in states if not s['oneoff'])
        cleanup['active_oneoff_containers']=[s['container'] for s in states if s['oneoff'] and (s['running'] or s['status'] not in ('created','exited','dead'))]
        retry_safe=cleanup['core_gateway_confirmed_stopped'] and not cleanup['active_oneoff_containers']
    except (Refused,subprocess.TimeoutExpired,OSError,ValueError,KeyError,KeyboardInterrupt) as exc:
        cleanup.update(core_gateway_confirmed_stopped=None,inspection_error=str(exc) or type(exc).__name__)
        retry_safe=False
    detail={'upgrade_status':'failed','phase':phase,
            'configuration_preparation_confirmed_complete':phase=='service_start',
            'cause':str(cause) or type(cause).__name__, 'cleanup':cleanup,
            'persistent_sources_retained':True,'retry_after_correction_safe':retry_safe,
            'next_action':('Correct the reported cause and rerun the same command.' if retry_safe else
                           'Service or one-off state is running or unconfirmed. Inspect this project and stop its core/gateway; confirm preparation has exited before retrying.')}
    raise Refused(json.dumps(detail,ensure_ascii=False)) from cause

def run_upgrade(compose, project, run=command):
    try:
        run([*compose,'stop','core','gateway'])
    except (Refused,subprocess.TimeoutExpired,OSError,KeyboardInterrupt) as exc:
        fail_upgrade('initial_stop',exc,compose,project,run,False)
    try:
        run([*compose,'run','--rm','--no-deps','--pull','never','core','prepare-config',
             '--home','/var/lib/lantai','--config-template','/etc/lantai/config.template.yaml'])
    except (Refused,subprocess.TimeoutExpired,OSError,KeyboardInterrupt) as exc:
        fail_upgrade('configuration_preparation',exc,compose,project,run,False)
    try:
        run([*compose,'up','--detach','--no-build','--pull','never','--wait','--wait-timeout','45'])
    except (Refused,subprocess.TimeoutExpired,OSError,KeyboardInterrupt) as exc:
        fail_upgrade('service_start',exc,compose,project,run,True)

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--project',required=True)
    parser.add_argument('--env-file',type=Path,required=True)
    parser.add_argument('--compose',type=Path,required=True)
    parser.add_argument('--check-only',action='store_true')
    args=parser.parse_args()
    if not re.fullmatch(r'[a-z0-9][a-z0-9_-]*',args.project):
        raise Refused('invalid existing Compose project name')
    compose_path=args.compose.resolve(); env_path=args.env_file.resolve()
    if not compose_path.is_file() or not env_path.is_file():
        raise Refused('existing environment file and reviewed new Compose file are required')
    compose=['docker','compose','--project-name',args.project,'--project-directory',str(compose_path.parent),
             '--env-file',str(env_path),'-f',str(compose_path)]
    config=json.loads(command([*compose,'config','--format','json']))
    ids=command(['docker','ps','--all','--quiet','--filter','label=com.docker.compose.project='+args.project,
                 '--filter','label=com.docker.compose.service=core']).split()
    if not ids:
        raise Refused('no existing core in this project; this launcher is for an existing installation')
    records=json.loads(command(['docker','inspect',*ids]))
    records=[r for r in records if r['Config'].get('Labels',{}).get('com.docker.compose.oneoff','false').lower()!='true']
    if len(records)!=1:
        raise Refused('expected exactly one existing core; no service stopped')
    image=json.loads(command(['docker','image','inspect',config['services']['core']['image']]))[0]
    facts=check_plan(config,records[0],image)
    print(json.dumps({'preflight':'passed',**facts}),flush=True)
    if args.check_only:
        return
    run_upgrade(compose,args.project)
    print(json.dumps({'configuration_migration':'passed','services_requested_running':True,
                      'retry_safe':True,'application_tls_and_browser_checks':'verify separately'}))

if __name__=='__main__':
    try:
        main()
    except (Refused,KeyError,ValueError,subprocess.TimeoutExpired,OSError) as exc:
        print('upgrade refused: '+str(exc),file=sys.stderr)
        sys.exit(1)
