import copy
import importlib.util
import unittest
from pathlib import Path
s=importlib.util.spec_from_file_location('upgrade',Path(__file__).with_name('upgrade-config.py'))
u=importlib.util.module_from_spec(s);s.loader.exec_module(u)
class UpgradePlanTests(unittest.TestCase):
 def fixture(self):
  paths=['/var/lib/lantai','/var/lib/lantai-secrets','/var/lib/lantai-setup']
  mounts=[{'type':'volume','source':'data','target':paths[0]}, {'type':'volume','source':'keys','target':paths[1]}, {'type':'bind','source':'/synthetic/setup','target':paths[2]}, {'type':'bind','source':'/synthetic/config.yaml','target':'/etc/lantai/config.template.yaml','read_only':True}]
  service={'user':'65532:65532','read_only':True,'cap_drop':['ALL'],'pull_policy':'never','networks':{'backend':None},'security_opt':['no-new-privileges=true'],'volumes':mounts}
  config={'services':{'core':service},'volumes':{'data':{'name':'existing_data'},'keys':{'name':'existing_keys'}},'networks':{'backend':{'internal':True}}}
  old={'Mounts':[{'Type':'volume','Name':'existing_data','Destination':paths[0]}, {'Type':'volume','Name':'existing_keys','Destination':paths[1]}, {'Type':'bind','Source':'/synthetic/setup','Destination':paths[2]}, {'Type':'bind','Source':'/synthetic/config.yaml','Destination':'/var/lib/lantai/config.yaml','RW':False}]}
  image={'Os':'linux','Architecture':'amd64','Id':'sha256:synthetic'}
  return config,old,image
 def test_legacy_and_retry_layout(self):
  c,o,i=self.fixture();self.assertEqual(u.check_plan(c,o,i)['layout'],'legacy_readonly_bind')
  o['Mounts'].pop();self.assertEqual(u.check_plan(c,o,i)['layout'],'owned_config')
 def test_refuses_changed_volume_key_config_and_permissions(self):
  for kind in ['data','keys','setup','template','privileged','cap_add','root','network','image']:
   with self.subTest(kind=kind):
    c,o,i=self.fixture();core=c['services']['core']
    if kind in ['data','keys']:c['volumes'][kind]['name']='other_'+kind
    elif kind=='setup':core['volumes'][2]['source']='/different/setup'
    elif kind=='template':core['volumes'][3]['source']='/different/config'
    elif kind=='privileged':core['privileged']=True
    elif kind=='cap_add':core['cap_add']=['DAC_OVERRIDE']
    elif kind=='root':core['user']='0:0'
    elif kind=='network':core['networks']={'edge':None}
    elif kind=='image':i['Architecture']='arm64'
    with self.assertRaises(u.Refused):u.check_plan(c,o,i)



class UpgradeFailureTests(unittest.TestCase):
 def compose(self):return ['docker','compose','--project-name','synthetic-target','-f','reviewed.yaml']
 def runner(self,fail_phase=None,stop_error=False,inspect_error=False,still_running=False):
  calls=[];up_attempts=[0];stops=[0]
  def run(args):
   calls.append(args)
   if args[:2]==['docker','ps']:return 'target-core target-gateway'
   if args[:2]==['docker','inspect']:
    if inspect_error:raise u.Refused('daemon unavailable')
    import json
    return json.dumps([{'Id':'target-'+name,'Config':{'Labels':{'com.docker.compose.project':'synthetic-target','com.docker.compose.service':name}},'State':{'Running':still_running,'Status':'running' if still_running else 'exited'}} for name in ['core','gateway']])
   if 'stop' in args:
    stops[0]+=1
    if fail_phase=='initial_stop' or stop_error and stops[0]>1:raise u.Refused('stop denied')
   if 'prepare-config' in args and fail_phase=='configuration_preparation':raise u.Refused('missing matching key')
   if 'up' in args:
    up_attempts[0]+=1
    if up_attempts[0]==1:
     if fail_phase=='service_start':raise u.Refused('gateway unhealthy after core started')
     if fail_phase=='timeout':raise u.subprocess.TimeoutExpired(args,120)
     if fail_phase=='interrupt':raise KeyboardInterrupt()
   return ''
  return run,calls
 def failure(self,run):
  import json
  with self.assertRaises(u.Refused) as c:u.run_upgrade(self.compose(),'synthetic-target',run)
  return json.loads(str(c.exception))
 def test_migration_failure_does_not_call_up(self):
  run,calls=self.runner('configuration_preparation');r=self.failure(run)
  self.assertEqual(r['phase'],'configuration_preparation');self.assertFalse(r['cleanup']['attempted']);self.assertTrue(r['retry_after_correction_safe'])
  self.assertFalse(any('up' in a for a in calls));self.assertEqual(sum('stop' in a for a in calls),1)
 def test_partial_start_cleanup_exact_scope_and_same_command_retry(self):
  run,calls=self.runner('service_start');r=self.failure(run)
  self.assertEqual(r['phase'],'service_start');self.assertTrue(r['configuration_preparation_confirmed_complete']);self.assertTrue(r['cleanup']['core_gateway_confirmed_stopped']);self.assertTrue(r['retry_after_correction_safe'])
  stops=[a for a in calls if 'stop' in a];self.assertEqual(stops,[self.compose()+['stop','core','gateway']]*2)
  self.assertFalse(any('down' in a or 'rm' in a for a in calls))
  u.run_upgrade(self.compose(),'synthetic-target',run)
 def test_timeout_and_interrupt_cleanup(self):
  for phase in ['timeout','interrupt']:
   with self.subTest(phase=phase):
    run,calls=self.runner(phase);r=self.failure(run);self.assertTrue(r['cleanup']['attempted']);self.assertTrue(r['cleanup']['core_gateway_confirmed_stopped'])
 def test_failed_stop_does_not_claim_services_stopped(self):
  run,_=self.runner('service_start',stop_error=True,still_running=True);r=self.failure(run)
  self.assertFalse(r['cleanup']['stop_succeeded']);self.assertFalse(r['cleanup']['core_gateway_confirmed_stopped']);self.assertFalse(r['retry_after_correction_safe']);self.assertIn('unhealthy',r['cause'])
 def test_failed_inspection_reports_unknown(self):
  run,_=self.runner('service_start',inspect_error=True);r=self.failure(run)
  self.assertIsNone(r['cleanup']['core_gateway_confirmed_stopped']);self.assertFalse(r['retry_after_correction_safe'])
 def test_initial_stop_failure_does_not_migrate(self):
  run,calls=self.runner('initial_stop',still_running=True);r=self.failure(run)
  self.assertEqual(r['phase'],'initial_stop');self.assertFalse(r['retry_after_correction_safe']);self.assertFalse(any('prepare-config' in a or 'up' in a for a in calls))

class CommandProcessGroupTests(unittest.TestCase):
 def test_timeout_terminates_only_own_cli_process_group(self):
  from unittest.mock import MagicMock,patch
  process=MagicMock();process.pid=4242;process.communicate.side_effect=[u.subprocess.TimeoutExpired(['synthetic-cli'],120),('','')]
  popen=MagicMock();popen.__enter__.return_value=process
  with patch.object(u.subprocess,'Popen',return_value=popen) as start,patch.object(u.os,'killpg') as kill:
   with self.assertRaises(u.subprocess.TimeoutExpired):u.command(['synthetic-cli'])
   self.assertTrue(start.call_args.kwargs['start_new_session']);kill.assert_called_once_with(4242,u.signal.SIGKILL)
   self.assertEqual(process.communicate.call_count,2)

if __name__=='__main__':unittest.main()
