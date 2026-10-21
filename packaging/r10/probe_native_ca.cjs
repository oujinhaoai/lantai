const fs=require('fs'),os=require('os'),path=require('path'),cp=require('child_process'),https=require('https');
const {chromium}=require('../../web/node_modules/@playwright/test');
const {trustLocalTestCA}=require('./browser_adapter.cjs');
(async()=>{
 const work=fs.mkdtempSync(path.join(os.tmpdir(),'r10-native-ca-probe-'));fs.chmodSync(work,0o700);
 const ca=path.join(work,'cert.pem'),key=path.join(work,'key.pem');
 cp.execFileSync('openssl',['req','-x509','-newkey','ec','-pkeyopt','ec_paramgen_curve:P-256','-nodes','-days','1','-subj','/CN=localhost','-addext','subjectAltName=DNS:localhost,IP:127.0.0.1','-addext','basicConstraints=critical,CA:TRUE','-addext','keyUsage=critical,digitalSignature,keyCertSign,cRLSign','-keyout',key,'-out',ca],{stdio:'pipe'});fs.chmodSync(key,0o600);
 process.env.CANDIDATE_BROWSER_PROOF=path.join(work,'native-proof.json');
 process.env.CANDIDATE_SYNTHETIC_CA=ca;
 let context,server;const result={status:'running',host_policy_changed:false,host_trust_store_changed:false,private_profile_only:true};
 try{
  const profile=path.join(work,'profile');fs.mkdirSync(profile,{mode:0o700});
  context=await chromium.launchPersistentContext(profile,{executablePath:'/opt/google/chrome/chrome',chromiumSandbox:true,ignoreHTTPSErrors:false,locale:'en-US',args:['--lang=en-US']});
  await trustLocalTestCA(context,ca);
  server=https.createServer({key:fs.readFileSync(key),cert:fs.readFileSync(ca)},(q,r)=>r.end('synthetic strict TLS'));
  await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));const page=await context.newPage();
  await page.goto(`https://localhost:${server.address().port}`);if(await page.locator('body').innerText()!=='synthetic strict TLS')throw Error('Synthetic strict TLS response mismatch');
  result.status='passed';
 }catch(e){result.status='failed';result.error_type=e.name;result.message=e.message}
 finally{
  if(fs.existsSync(process.env.CANDIDATE_BROWSER_PROOF))result.native_proof=JSON.parse(fs.readFileSync(process.env.CANDIDATE_BROWSER_PROOF));
  try{cp.execFileSync('python3',[path.join(__dirname,'native_file_dialog.py'),ca,'--cancel'],{timeout:5000,stdio:'pipe'})}catch{}
  if(context)await context.close();if(server)await new Promise(resolve=>server.close(resolve));fs.rmSync(work,{recursive:true,force:true});
 }
 fs.writeFileSync(process.argv[2],JSON.stringify(result,null,2)+'\n');console.log('NATIVE_CA_PROBE '+JSON.stringify(result));process.exitCode=result.status==='passed'?0:1;
})().catch(e=>{console.log(JSON.stringify({status:'failed',error_type:e.name,message:e.message}));process.exitCode=1});
