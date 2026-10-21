"""Real isolated upgrade/recovery using existing, immutable image artifacts only."""
import argparse, base64, copy, hashlib, hmac, importlib.util, json, os
from pathlib import Path
import re, secrets, select, struct, subprocess, sys, tempfile, time

ROOT=Path(__file__).resolve().parents[2]
sys.path.insert(0,str(ROOT/'scripts'))
spec=importlib.util.spec_from_file_location('delivery',ROOT/'scripts/test-release-delivery.py')
d=importlib.util.module_from_spec(spec);spec.loader.exec_module(d)
EXPECTED='3ad86ea10830676d5ef5d72286a02fd0d9d01c65'
BASELINE='d27a06fe0af55e205a19e8f95f0764167e217741'

def save(p,v):p.write_text(json.dumps(v,ensure_ascii=False,indent=2)+'\n')
def otp(secret):
    value=hmac.new(secret,struct.pack('>Q',int(time.time())//30),hashlib.sha1).digest();offset=value[-1]&15
    return str((struct.unpack('>I',value[offset:offset+4])[0]&0x7fffffff)%1000000).zfill(6)
def sha(p):return hashlib.sha256(p.read_bytes()).hexdigest()
def verify_sums(root):
    for line in (root/'SHA256SUMS').read_text().splitlines():
        digest,name=line.split('  ',1);assert '/' not in name and (root/name).is_file() and sha(root/name)==digest
def worker(cid,image,extra=()):
    return ['docker','run','--rm','--network','none','--read-only','--user','65532:65532',
        '--cap-drop','ALL','--security-opt','no-new-privileges:true','--tmpfs','/tmp:rw,noexec,nosuid,nodev,mode=1777,size=64m',
        '--volumes-from',cid,*extra,image]
def raw(argv,expected=None):
    r=subprocess.run(argv,text=True,capture_output=True,env=d.ENV,timeout=300)
    if expected is not None:assert r.returncode==expected,'unexpected isolated command status'
    return r
def doctor(base):
    r=raw(base+['doctor','-home','/var/lib/lantai','-json']);return r.returncode,json.loads(r.stdout)
def runtime(report):return next(x['applied_version'] for x in report['databases'] if x['database']=='runtime')
def last_json(text):
    decoder=json.JSONDecoder();text=text.strip();value=None
    while text:
        value,end=decoder.raw_decode(text);text=text[end:].lstrip()
    return value

def reset_admin(base,name):
    password=secrets.token_urlsafe(28);d.PRIVATE_VALUES.append(password)
    interactive=base[:2]+['--interactive']+base[2:]
    process=subprocess.Popen(interactive+['recover-admin','-home','/var/lib/lantai','-admin',name,
        '-note','synthetic isolated backup restoration'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,env=d.ENV)
    process.stdin.write(('RESET '+name+'\n'+password+'\n'+password+'\n').encode());process.stdin.flush()
    output=b'';deadline=time.monotonic()+90
    while b'Enter the current 6-digit code' not in output and time.monotonic()<deadline:
        ready,_,_=select.select([process.stdout],[],[],1)
        if ready:
            chunk=os.read(process.stdout.fileno(),4096)
            if not chunk:break
            output+=chunk
    match=re.search(rb'  secret  ([A-Z2-7]+=*)',output)
    if not match:
        process.kill();process.wait();raise RuntimeError('synthetic recovery enrollment unavailable; private transcript excluded')
    secret=base64.b32decode(match[1]);d.PRIVATE_VALUES.append(match[1].decode())
    process.stdin.write((otp(secret)+'\n').encode());process.stdin.flush()
    remaining,_=process.communicate(timeout=90);output+=remaining
    assert process.returncode==0,'synthetic recovery confirmation failed; private transcript excluded'
    # Never publish enrollment, passwords, recovery codes or the transcript.
    return password,secret

def main():
    ap=argparse.ArgumentParser();ap.add_argument('--candidate',type=Path,required=True);ap.add_argument('--baseline',type=Path,required=True);ap.add_argument('--out',type=Path,required=True);a=ap.parse_args()
    a.out.mkdir(mode=0o700);work=Path(tempfile.mkdtemp(prefix='lantai-migration-'));work.chmod(0o700)
    d.RESULT.update(schema='lantai.isolated-migration-recovery/v1',status='running',
        candidate_image_commit=EXPECTED,baseline_commit=BASELINE,product_source_modified=False,
        NAS_accessed=False,real_data_used=False,browser_retested=False,checks=[])
    instances=[]
    try:
        d.require_local_docker(d.run)
        new=json.loads((a.candidate/'manifest.json').read_text());old=json.loads((a.baseline/'manifest.json').read_text())
        assert new['source_commit']==EXPECTED and old['source_commit']==BASELINE
        verify_sums(a.candidate);d.verify_files(a.baseline,old)
        d.load_image(a.baseline,old);d.load_image(a.candidate,new)
        d.run(['docker','load','-i',str(a.candidate/'caddy-2.11.4-linux-amd64-image.tar.gz')])
        d.RESULT['candidate_core_image']=new['core_image'];d.RESULT['baseline_core_image']=old['core_image']
        old_web,old_deploy=work/'old-web',work/'old-deploy'
        for item,target in ((old['web'],old_web),(old['deployment'],old_deploy)):
            d.extract_verified_archive(a.baseline/item['archive'],target,item['archive_prefix'],item['files'])
        web=work/'new-web';d.extract_verified_archive(a.candidate/new['web']['archive'],web,new['web']['archive_prefix'],new['web']['files'])
        old_cli,new_cli=work/'old-lantai',work/'new-lantai';d.binary_from(a.baseline,old,old_cli)
        image_container=d.run(['docker','create',new['core_image']['tag']])
        try:d.run(['docker','cp',image_container+':/usr/local/bin/lantai',str(new_cli)])
        finally:d.run(['docker','rm',image_container])
        new_cli.chmod(0o700)
        source=d.Instance(work/'source',old_deploy,old_web,old['caddy_image']['tag'],
            (old_deploy/'compose.yaml').read_text(),(old_deploy/'Caddyfile').read_bytes(),old['core_image']['tag'])
        source.compose+=['-f',str(source.root/'compose.web.yaml')];source.enable_backup(old_deploy);instances.append(source)
        d.RESULT['phase']='initialize actual old release and capture verified backup'
        d.run(source.compose+['up','-d']);source.initialize_and_seed(old_cli);source.retained(old_cli,old);source.ready();source.tls_negative()
        job=source.backup_smoke(old);source.stop();source_cid=source.cid()
        old_worker=worker(source_cid,old['core_image']['tag']);new_worker=worker(source_cid,new['core_image']['tag'])
        backup_path='/var/lib/lantai-backup/'+job
        backup=json.loads(d.run(old_worker+['backup-verify','-backup',backup_path]))
        d.check('verified backup preserves schema14',backup['matrix']['databases']['runtime']==14 and backup['instance_id']==source.meta['instance_id'])
        before_schema,before_keys=source.snapshot('pre-migration')
        d.RESULT['phase']='negative migration guards before schema changes'
        denied=raw(new_worker+['migrate','-home','/var/lib/lantai','-json'])
        d.check('missing verified backup rejected',denied.returncode!=0 and 'verified complete backup' in denied.stderr)
        denied=raw(new_worker+['migrate','-home','/var/lib/lantai','-backup','/var/lib/lantai-backup/absent','-json'])
        d.check('absent backup rejected',denied.returncode!=0)
        _,report=doctor(new_worker);d.check('negative guards retain schema14 and no migration binding',runtime(report)==14 and report['marker'].get('migration') is None)
        # Preserve the existing source volumes and host config. Only the
        # reviewed template command/mount changes and new guarded service differ.
        config=json.loads(d.run(source.compose+['config','--format','json']))
        volumes=copy.deepcopy(config['services']['core']['volumes'])
        for v in volumes:
            if v['target']=='/var/lib/lantai-backup':v['read_only']=True
        migrate={'image':new['core_image']['tag'],'pull_policy':'never','command':['migrate','-home','/var/lib/lantai','-backup',backup_path,'-json'],
            'user':'65532:65532','read_only':True,'cap_drop':['ALL'],'security_opt':['no-new-privileges:true'],
            'network_mode':'none','tmpfs':['/tmp:rw,noexec,nosuid,nodev,mode=1777,size=64m'],'volumes':volumes,
            'depends_on':{'prepare':{'condition':'service_completed_successfully'}},'restart':'no'}
        overlay=source.root/'compose.migration.json';save(overlay,{'services':{'schema-migrate':migrate,'core':{'depends_on':{'schema-migrate':{'condition':'service_completed_successfully'}}}}})
        text=(source.root/'compose.yaml').read_text();old_command='command: [start, --home, /var/lib/lantai, --setup-dir, /var/lib/lantai-setup]'
        assert text.count(old_command)==1 and text.count(':/var/lib/lantai/config.yaml:ro')==1
        (source.root/'compose.yaml').write_text(text.replace(old_command,old_command[:-1]+', --config-template, /etc/lantai/config.template.yaml]').replace(':/var/lib/lantai/config.yaml:ro',':/etc/lantai/config.template.yaml:ro'))
        source.web=web;source.select(new['core_image']['tag']);source.compose+=['-f',str(overlay)]
        d.RESULT['phase']='one-up Compose guarded migration and normal service'
        d.run(source.compose+['up','-d']);source.retained(new_cli,new);source.ready();source.tls_negative();source.static(new['web']['files'])
        migration_cid=d.run(source.compose+['ps','-a','-q','schema-migrate']);migration=json.loads(d.run(['docker','logs',migration_cid]))
        d.check('Compose migrates runtime14 to15 before core starts',migration['before']['runtime']==14 and migration['after']['runtime']==15 and migration['applied']['runtime']==[15])
        d.RESULT['migration_report']=migration;source.stop();after_schema,after_keys=source.snapshot('post-migration')
        d.check('migration preserves original master keys',before_keys==after_keys)
        d.check('only runtime schema changes',all(after_schema[k]==v for k,v in before_schema.items() if k!='db/runtime.db') and before_schema['db/runtime.db']!=after_schema['db/runtime.db'])
        d.run(source.compose+['up','-d']);source.retained(new_cli,new);source.ready()
        migration_cid=d.run(source.compose+['ps','-a','-q','schema-migrate']);replay=last_json(d.run(['docker','logs',migration_cid]))
        d.check('stopped GUI rebuild is migration idempotent',replay['noop'] is True)
        source.stop();source_cid=source.cid();new_worker=worker(source_cid,new['core_image']['tag']);old_worker=worker(source_cid,old['core_image']['tag'])
        _,report=doctor(new_worker);d.check('migrated instance is compatible at15',report['compatible'] and runtime(report)==15)
        status,old_report=doctor(old_worker);d.check('old image cannot downgrade schema15',status!=0 and not old_report['compatible'])
        d.RESULT['forward_migration_passed']=True;d.RESULT['same_volume_old_image_rollback_supported']=False
        # Independent restored volumes, original backup and separately mounted
        # original key. The migrated source data volume is never a restore target.
        d.RESULT['phase']='restore verified schema14 backup to independent volumes'
        restored=d.Instance(work/'restored',old_deploy,old_web,old['caddy_image']['tag'],
            (old_deploy/'compose.yaml').read_text(),(old_deploy/'Caddyfile').read_bytes(),old['core_image']['tag'])
        restored.port=source.port;restored.origin=source.origin;restored.config.write_bytes(source.config.read_bytes());restored.config_hash=sha(restored.config);restored.select(old['core_image']['tag'])
        restored.compose+=['-f',str(restored.root/'compose.web.yaml')];instances.append(restored)
        d.run(restored.compose+['up','--no-deps','prepare'])
        prepare_cid=d.run(restored.compose+['ps','-a','-q','prepare'])
        key_volume=source.mounts()['/var/lib/lantai-secrets']['source']
        extra=['--mount',f'type=bind,source={source.backup_root},target=/var/lib/lantai-backup,readonly',
            '--mount',f'type=volume,source={key_volume},target=/recovery-source-keys,readonly']
        recovery_worker=worker(prepare_cid,old['core_image']['tag'],extra)
        marker=json.loads(d.run(recovery_worker+['restore','-home','/var/lib/lantai','-backup',backup_path,'-key-dir','/recovery-source-keys','-minimum-epoch',str(backup['recovery_epoch'])]))
        status,report=doctor(recovery_worker)
        d.check('restore retains14 and remains closed pending new credentials',status!=0 and runtime(report)==14 and report['marker']['restore'] is not None)
        d.check('restore advances recovery epoch',marker['recovery_epoch']>backup['recovery_epoch'])
        restored.password,restored.factor=reset_admin(recovery_worker,source.name);restored.name=source.name;restored.last_step=int(time.time())//30
        proof=b'Synthetic isolated rollback rehearsal: one known pre-upgrade resource; no post-capture business writes; old credentials revoked; source retained separately.'
        proof_path=work/'restore-evidence.txt';proof_path.write_bytes(proof);proof_path.chmod(0o644)
        r=marker['restore'];review={'contract':'lantai.recovery-review/v1','run_id':r['run_id'],'backup_id':r['backup_id'],'manifest_digest':r['manifest_digest'],
            'recovery_epoch':marker['recovery_epoch'],'administrator':source.who['principal']['principal_id'],
            'revocations_reconciled':True,'deletions_reconciled':True,'open_operations_reconciled':True,'evidence_digest':hashlib.sha256(proof).hexdigest(),'note':'synthetic isolated rollback evidence'}
        review_path=work/'review.json';save(review_path,review);review_path.chmod(0o644)
        complete_worker=worker(prepare_cid,old['core_image']['tag'],extra+['--mount',f'type=bind,source={proof_path},target=/evidence.txt,readonly','--mount',f'type=bind,source={review_path},target=/review.json,readonly'])
        d.run(complete_worker+['restore-complete','-home','/var/lib/lantai','-review','/review.json','-evidence','/evidence.txt'])
        status,report=doctor(recovery_worker);d.check('verified old-format recovery completes',status==0 and report['compatible'] and runtime(report)==14)
        d.run(restored.compose+['up','-d']);restored.wait('/api/v1/meta',200);restored.ready();restored.tls_negative()
        d.check('source session is invalid after restored epoch',restored.request('/api/v1/whoami',credential=source.token)[0]==401)
        status,body,_=restored.request('/api/v1/sessions/login',{'name':restored.name,'password':restored.password,'code':restored.totp(),'channel':'cli','scopes':['admin','read','ingest','organize','self'],'ttl_seconds':900})
        d.check('restored original administrator uses fresh factor',status==201)
        restored.token=json.loads(body)['token'];d.PRIVATE_VALUES.append(restored.token);restored.session=restored.root/'session.json'
        save(restored.session,{'schema':'lantai.client-session/v1','origin':restored.origin,'token':restored.token});restored.session.chmod(0o600)
        restored.who=source.who;restored.meta=source.meta;restored.receipt=source.receipt;restored.content=source.content
        restored.retained(old_cli,old);restored.stop();d.run(restored.compose+['up','-d']);restored.retained(old_cli,old);restored.ready()
        d.check('recovery target volumes differ from migrated source',restored.mounts()['/var/lib/lantai']['source']!=source.mounts()['/var/lib/lantai']['source'] and restored.mounts()['/var/lib/lantai-secrets']['source']!=key_volume)
        d.RESULT.update(status='passed',independent_schema14_restore_passed=True,original_identity_version_bytes_preserved=True,
            gui_forward_compose_one_up_verified=True,gui_old_version_recovery_without_CLI_verified=False,
            GUI_recovery_gap='Old format recovery still needs interactive recover-admin and explicit restore-complete review; these were exercised by isolated driver, not a NAS GUI.')
    except Exception as error:
        message=str(error)
        for v in d.PRIVATE_VALUES:
            if v:message=message.replace(v,'[REDACTED]')
        d.RESULT.update(status='failed',error=type(error).__name__+': '+message)
    finally:
        for instance in instances:
            try:instance.stop()
            except Exception:d.RESULT.update(cleanup_failed=True,status='failed')
        d.RESULT['test_secrets_databases_keys_backups_uploaded']=False
        save(a.out/'MIGRATION-RESULT.json',d.RESULT)
    print(json.dumps({'status':d.RESULT['status'],'checks':len(d.RESULT['checks']),'error':d.RESULT.get('error'),'report':'MIGRATION-RESULT.json'}),flush=True)
    return 0 if d.RESULT['status']=='passed' else 1

if __name__=='__main__':sys.exit(main())
