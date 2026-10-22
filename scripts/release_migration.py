"""Synthetic schema-14 to schema-15 upgrade and independent recovery checks.

Derived from the frozen r10 rehearsal; the caller supplies verified artifacts.
Never use this harness against an existing deployment.
"""
import base64, hashlib, hmac, json, os
import re, secrets, select, struct, subprocess, time

from release_upgrade import assert_retained_upgrade

def save(p,v):p.write_text(json.dumps(v,ensure_ascii=False,indent=2)+'\n')
def otp(secret):
    value=hmac.new(secret,struct.pack('>Q',int(time.time())//30),hashlib.sha1).digest();offset=value[-1]&15
    return str((struct.unpack('>I',value[offset:offset+4])[0]&0x7fffffff)%1000000).zfill(6)
def sha(p):return hashlib.sha256(p.read_bytes()).hexdigest()
def worker(cid,image,extra=()):
    return ['docker','run','--rm','--network','none','--read-only','--user','65532:65532',
        '--cap-drop','ALL','--security-opt','no-new-privileges:true','--tmpfs','/tmp:rw,noexec,nosuid,nodev,mode=1777,size=64m',
        '--volumes-from',cid,*extra,image]
def raw(d,argv,expected=None):
    r=subprocess.run(argv,text=True,capture_output=True,env=d.ENV,timeout=300)
    if expected is not None:assert r.returncode==expected,'unexpected isolated command status'
    return r
def doctor(d,base):
    r=raw(d,base+['doctor','-home','/var/lib/lantai','-json']);return r.returncode,json.loads(r.stdout)
def runtime(report):return next(x['applied_version'] for x in report['databases'] if x['database']=='runtime')
def reset_admin(d,base,name):
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

def run_migration_recovery(d,source,new,old,web,deployment,old_web,old_deploy,new_cli,old_cli,work,evidence,instances,no_create_verified):
    # Enable the verified backup path on this synthetic old instance before capture.
    source.enable_backup(old_deploy);source.select(old['core_image']['tag'])
    d.run(source.compose+['up','-d']);source.retained(old_cli,old);source.ready()
    job=source.backup_smoke(old);source.stop();source_cid=source.cid()
    old_worker=worker(source_cid,old['core_image']['tag']);new_worker=worker(source_cid,new['core_image']['tag'])
    backup_path='/var/lib/lantai-backup/'+job
    backup=json.loads(d.run(old_worker+['backup-verify','-backup',backup_path]))
    d.check('verified backup preserves schema14',backup['matrix']['databases']['runtime']==14 and backup['instance_id']==source.meta['instance_id'])
    before_schema,before_keys=source.snapshot('pre-migration')
    d.RESULT['phase']='negative migration guards before schema changes'
    denied=raw(d,new_worker+['migrate','-home','/var/lib/lantai','-json'])
    d.check('missing verified backup rejected',denied.returncode!=0 and 'verified complete backup' in denied.stderr)
    denied=raw(d,new_worker+['migrate','-home','/var/lib/lantai','-backup','/var/lib/lantai-backup/absent','-json'])
    d.check('absent backup rejected',denied.returncode!=0)
    _,report=doctor(d,new_worker);d.check('negative guards retain schema14 and no migration binding',runtime(report)==14 and report['marker'].get('migration') is None)
    # Preserve the existing source volumes and host config. Only the
    # reviewed template command/mount changes differ; migration runs offline.
    before_config=json.loads(d.run(source.compose+['config','--format','json']))
    mounts=source.mounts()
    text=(source.root/'compose.yaml').read_text();old_command='command: [start, --home, /var/lib/lantai, --setup-dir, /var/lib/lantai-setup]'
    assert text.count(old_command)==1 and text.count(':/var/lib/lantai/config.yaml:ro')==1
    (source.root/'compose.yaml').write_text(text.replace(old_command,old_command[:-1]+', --config-template, /etc/lantai/config.template.yaml]').replace(':/var/lib/lantai/config.yaml:ro',':/etc/lantai/config.template.yaml:ro'))
    source.web=web;source.change_caddy(new['caddy_image']['tag']);source.select(new['core_image']['tag'])
    (source.root/'compose.web.yaml').write_bytes((deployment/'compose.web.yaml').read_bytes())
    source.caddy_file.write_bytes((deployment/'Caddyfile').read_bytes())
    after_config=json.loads(d.run(source.compose+['config','--format','json']))
    assert_retained_upgrade(before_config,after_config,core_image=new['core_image']['tag'],caddy_image=new['caddy_image']['tag'],web=web,backup=source.backup_root,root_id=source.backup_root_id,no_create_verified=no_create_verified,backup_preconfigured=True,config_template_migration=True)
    d.check('migration preserves protected project, volumes, TLS, environment and permissions')
    d.RESULT['phase']='offline verified-backup migration before normal service'
    migration=json.loads(d.run(new_worker+['migrate','-home','/var/lib/lantai','-backup',backup_path,'-json']))
    d.check('offline worker migrates runtime14 to15',migration['before']['runtime']==14 and migration['after']['runtime']==15 and migration['applied']['runtime']==[15])
    d.RESULT['migration_report']=migration;after_schema,after_keys=source.snapshot('post-migration')
    d.check('migration preserves original master keys',before_keys==after_keys)
    d.check('only runtime schema changes',all(after_schema[k]==v for k,v in before_schema.items() if k!='db/runtime.db') and before_schema['db/runtime.db']!=after_schema['db/runtime.db'])
    replay=json.loads(d.run(new_worker+['migrate','-home','/var/lib/lantai','-backup',backup_path,'-json']))
    d.check('offline migration replay is idempotent',replay['noop'] is True)
    d.run(source.compose+['up','-d']);source.retained(new_cli,new);source.ready();source.tls_negative();source.static(new['web']['files']);source.browser(evidence,'migrated-upgrade');source.reduced_session();source.backup_smoke(new)
    current_mounts=source.mounts();legacy_config=mounts.pop('/var/lib/lantai/config.yaml')
    d.check('migration retains original data, key and setup mounts',current_mounts==mounts)
    d.check('migration retains original config source as read-only template',any(v.get('target')=='/etc/lantai/config.template.yaml' and v.get('source')==legacy_config['source'] and v.get('read_only') is True for v in after_config['services']['core']['volumes']))
    source.stop();source_cid=source.cid();new_worker=worker(source_cid,new['core_image']['tag']);old_worker=worker(source_cid,old['core_image']['tag'])
    _,report=doctor(d,new_worker);d.check('migrated instance is compatible at15',report['compatible'] and runtime(report)==15)
    status,old_report=doctor(d,old_worker);d.check('old image cannot downgrade schema15',status!=0 and not old_report['compatible'])
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
    status,report=doctor(d,recovery_worker)
    d.check('restore retains14 and remains closed pending new credentials',status!=0 and runtime(report)==14 and report['marker']['restore'] is not None)
    d.check('restore advances recovery epoch',marker['recovery_epoch']>backup['recovery_epoch'])
    restored.password,restored.factor=reset_admin(d,recovery_worker,source.name);restored.name=source.name;restored.last_step=int(time.time())//30
    proof=b'Synthetic isolated rollback rehearsal: one known pre-upgrade resource; no post-capture business writes; old credentials revoked; source retained separately.'
    proof_path=work/'restore-evidence.txt';proof_path.write_bytes(proof);proof_path.chmod(0o644)
    r=marker['restore'];review={'contract':'lantai.recovery-review/v1','run_id':r['run_id'],'backup_id':r['backup_id'],'manifest_digest':r['manifest_digest'],
        'recovery_epoch':marker['recovery_epoch'],'administrator':source.who['principal']['principal_id'],
        'revocations_reconciled':True,'deletions_reconciled':True,'open_operations_reconciled':True,'evidence_digest':hashlib.sha256(proof).hexdigest(),'note':'synthetic isolated rollback evidence'}
    review_path=work/'review.json';save(review_path,review);review_path.chmod(0o644)
    complete_worker=worker(prepare_cid,old['core_image']['tag'],extra+['--mount',f'type=bind,source={proof_path},target=/evidence.txt,readonly','--mount',f'type=bind,source={review_path},target=/review.json,readonly'])
    d.run(complete_worker+['restore-complete','-home','/var/lib/lantai','-review','/review.json','-evidence','/evidence.txt'])
    status,report=doctor(d,recovery_worker);d.check('verified old-format recovery completes',status==0 and report['compatible'] and runtime(report)==14)
    d.run(restored.compose+['up','-d']);restored.wait('/api/v1/meta',200);restored.ready();restored.tls_negative()
    d.check('source session is invalid after restored epoch',restored.request('/api/v1/whoami',credential=source.token)[0]==401)
    status,body,_=restored.request('/api/v1/sessions/login',{'name':restored.name,'password':restored.password,'code':restored.totp(),'channel':'cli','scopes':['admin','read','ingest','organize','self'],'ttl_seconds':900})
    d.check('restored original administrator uses fresh factor',status==201)
    restored.token=json.loads(body)['token'];d.PRIVATE_VALUES.append(restored.token);restored.session=restored.root/'session.json'
    save(restored.session,{'schema':'lantai.client-session/v1','origin':restored.origin,'token':restored.token});restored.session.chmod(0o600)
    restored.who=source.who;restored.meta=source.meta;restored.receipt=source.receipt;restored.content=source.content
    restored.retained(old_cli,old);restored.stop();d.run(restored.compose+['up','-d']);restored.retained(old_cli,old);restored.ready()
    d.check('recovery target volumes differ from migrated source',restored.mounts()['/var/lib/lantai']['source']!=source.mounts()['/var/lib/lantai']['source'] and restored.mounts()['/var/lib/lantai-secrets']['source']!=key_volume)
    d.RESULT.update(migration_upgrade_passed=True,independent_restore_passed=True,
        direct_upgrade_passed=False,rollback_passed=False,original_identity_preserved=True,
        original_totp_login_passed=True,static_archive_served=True,volumes_preserved=True,
        backup_smoke_passed=True,bind_no_create_passed=no_create_verified,
        same_volume_old_image_rollback_supported=False,
        upgrade_mode='runtime14-to15-offline-migration',
        recovery_scope='synthetic independent volumes with fresh credentials and explicit review; not NAS acceptance')
