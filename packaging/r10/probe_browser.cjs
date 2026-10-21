// Read-only diagnosis of a disposable native certificate manager profile.
const fs=require('fs'),os=require('os'),path=require('path'),crypto=require('crypto'),cp=require('child_process');
const {chromium}=require('../../web/node_modules/@playwright/test');
(async()=>{
 const binary='/opt/google/chrome/chrome',inventory='/var/lib/dpkg/info/google-chrome-stable.md5sums';
 const expected=fs.readFileSync(inventory,'utf8').split('\n').find(x=>x.trim().endsWith('opt/google/chrome/chrome'))?.trim().split(/\s+/)[0];
 if(!expected||crypto.createHash('md5').update(fs.readFileSync(binary)).digest('hex')!==expected)throw Error('Preinstalled browser differs from package inventory');
 const profile=fs.mkdtempSync(path.join(os.tmpdir(),'r10-native-ca-diagnostic-'));fs.chmodSync(profile,0o700);
 let context;const result={host_policy_changed:false,host_trust_store_changed:false,private_profile_only:true};
 try{
  context=await chromium.launchPersistentContext(profile,{executablePath:binary,chromiumSandbox:true,ignoreHTTPSErrors:false,locale:'en-US',args:['--lang=en-US']});
  result.version=context.browser().version();result.binary_sha256=crypto.createHash('sha256').update(fs.readFileSync(binary)).digest('hex');
  const page=await context.newPage();await page.goto('chrome://sandbox');result.sandbox_report=await page.locator('body').innerText();
  if(!/PID namespaces\s+Yes/.test(result.sandbox_report)||!/Seccomp-BPF sandbox\s+Yes/.test(result.sandbox_report))throw Error('Actual namespace/seccomp sandbox unavailable');
  result.pages=[];
  for(const url of ['chrome://certificate-manager/localcerts','chrome://certificate-manager/','chrome://settings/certificates']){
   const item={requested_url:url};
   try{
    await page.goto(url);await page.waitForTimeout(2500);item.url=page.url();item.title=await page.title();
    item.frames=page.frames().map(x=>x.url());
    item.tree=await page.evaluate(()=>{
     const out=[];function visit(n,depth){if(depth>24||out.length>500)return;
      if(n.nodeType===1){if(['STYLE','SCRIPT','LINK','META','SVG','PATH'].includes(n.tagName))return;
       const text=[...n.childNodes].filter(x=>x.nodeType===3).map(x=>x.textContent.trim()).filter(Boolean).join(' ').slice(0,220);
       out.push({depth,tag:n.tagName,id:n.id||undefined,role:n.getAttribute('role')||undefined,aria:n.getAttribute('aria-label')||undefined,text:text||undefined,shadow:n.shadowRoot?.mode});
       if(n.shadowRoot)visit(n.shadowRoot,depth+1);
      }for(const child of n.children||[])visit(child,depth+1);
     }visit(document.body,0);return out;
    });
    const client=await context.newCDPSession(page);const doc=await client.send('DOM.getDocument',{depth:-1,pierce:true});
    const tags={};function walk(n){tags[n.nodeName]=(tags[n.nodeName]||0)+1;for(const c of n.children||[])walk(c);for(const c of n.shadowRoots||[])walk(c);if(n.contentDocument)walk(n.contentDocument)}walk(doc.root);item.CDP_tag_counts=tags;
   }catch(error){item.error_type=error.name;item.message=error.message}
   result.pages.push(item);
  }
  result.status='diagnosed';
 }finally{if(context)await context.close();fs.rmSync(profile,{recursive:true,force:true});}
 const target=process.argv[2];fs.writeFileSync(target,JSON.stringify(result,null,2)+'\n');console.log(JSON.stringify(result));
})().catch(e=>{console.log(JSON.stringify({status:'failed',error_type:e.name,message:e.message}));process.exitCode=1});
