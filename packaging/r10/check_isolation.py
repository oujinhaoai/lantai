"""Probe the exact candidate's network namespace using private synthetic traffic."""
import argparse,hashlib,http.server,ipaddress,json,platform,subprocess,sys,tempfile,threading
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parent))
from test_migration import d,ROOT,EXPECTED,verify_sums,save
BODY=b'lantai synthetic isolation control'
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):self.send_response(200);self.end_headers();self.wfile.write(BODY)
    def log_message(self,*_):pass
def main():
    p=argparse.ArgumentParser();p.add_argument('--candidate',type=Path,required=True);p.add_argument('--out',type=Path,required=True);a=p.parse_args();a.out.mkdir(mode=0o700)
    work=Path(tempfile.mkdtemp(prefix='lantai-isolation-'));work.chmod(0o700);instance=None;server=None
    result={'schema':'lantai.exact-image-isolation/v1','status':'running','source_commit':EXPECTED,'NAS_accessed':False,'real_data_used':False,'checks':[]}
    try:
        assert platform.system()=='Linux' and platform.machine() in ('x86_64','amd64')
        d.require_local_docker(d.run);verify_sums(a.candidate);m=json.loads((a.candidate/'manifest.json').read_text());assert m['source_commit']==EXPECTED
        d.load_image(a.candidate,m);d.run(['docker','load','-i',str(a.candidate/'caddy-2.11.4-linux-amd64-image.tar.gz')])
        result.update(core_image=m['core_image'],host_architecture=platform.machine(),docker_server=d.run(['docker','version','--format','{{.Server.Version}}']),compose=d.run(['docker','compose','version','--short']))
        web=work/'web';d.extract_verified_archive(a.candidate/m['web']['archive'],web,m['web']['archive_prefix'],m['web']['files'])
        deploy=ROOT/'deploy';instance=d.Instance(work/'instance',deploy,web,m['caddy_image']['tag'],(deploy/'compose.yaml').read_text(),(deploy/'Caddyfile').read_bytes(),m['core_image']['tag'])
        instance.compose+=['-f',str(instance.root/'compose.web.yaml')];d.run(instance.compose+['up','-d']);instance.wait('/setup/',200)
        cid=instance.cid();info=d.inspect(cid);networks=list(info['NetworkSettings']['Networks']);assert len(networks)==1
        network=json.loads(d.run(['docker','network','inspect',networks[0]]))[0]
        assert info['Image']==m['core_image']['id'] and network['Internal'] is True and not info['HostConfig']['PortBindings']
        assert info['HostConfig']['ReadonlyRootfs'] and info['Config']['User']=='65532:65532' and 'ALL' in info['HostConfig']['CapDrop']
        result.update(core_internal_network=True,core_published_ports=False,core_read_only_root=True,core_user='65532:65532',core_cap_drop=['ALL'],verified_TLS_bootstrap_HTTP_status=200)
        bridge=json.loads(d.run(['docker','network','inspect','bridge']))[0];gateway=bridge['IPAM']['Config'][0]['Gateway'];assert ipaddress.ip_address(gateway).is_private
        server=http.server.ThreadingHTTPServer((gateway,0),Handler);threading.Thread(target=server.serve_forever,daemon=True).start();url=f'http://{gateway}:{server.server_port}/synthetic'
        def probe(mode):
            command=['docker','run','--rm','--pull=never','--network',mode,'--read-only','--user','65532:65532','--cap-drop','ALL','--security-opt','no-new-privileges:true','--entrypoint','/usr/bin/wget',m['caddy_image']['tag'],'-q','-T','2','-O','-',url]
            return subprocess.run(command,capture_output=True,env=d.ENV,timeout=15)
        control=probe('bridge');isolated=probe('container:'+cid)
        assert control.returncode==0 and control.stdout==BODY,'private bridge positive control did not connect'
        assert isolated.returncode!=0 and isolated.stdout!=BODY,'core network namespace reached external bridge control'
        result.update(status='passed',positive_control={'connected':True,'body_sha256':hashlib.sha256(control.stdout).hexdigest()},core_namespace_external_bridge_probe={'connected':False,'exit_code':isolated.returncode},external_Internet_or_NAS_contacted=False)
    except Exception as error:result.update(status='failed',error=type(error).__name__+': '+str(error))
    finally:
        if server:server.shutdown();server.server_close()
        if instance:
            try:instance.stop()
            except Exception:result.update(status='failed',cleanup_failed=True)
        save(a.out/'ISOLATION-RESULT.json',result)
    print('ISOLATION_RESULT '+json.dumps(result),flush=True);return 0 if result['status']=='passed' else 1
if __name__=='__main__':sys.exit(main())
