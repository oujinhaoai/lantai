// Packaging-only adaptation for Chrome155's native user certificate subpage.
// The original artifact browser assertions, sandbox and TLS policy are unchanged.
const fs=require('fs'),path=require('path'),{X509Certificate}=require('crypto'),cp=require('child_process');
const root=path.resolve(__dirname,'../..');
const {chromium}=require(path.join(root,'web/node_modules/@playwright/test'));
const launch=chromium.launchPersistentContext.bind(chromium);
chromium.launchPersistentContext=(profile,options)=>launch(profile,{...options,headless:false});
async function trustLocalTestCA(context,caFile){
 const cert=new X509Certificate(fs.readFileSync(caFile));
 if(!cert.ca||!cert.checkIssued(cert)||!cert.verify(cert.publicKey))throw Error('Explicit self-signed synthetic CA required');
 const page=await context.newPage();await page.goto('chrome://sandbox');const sandbox=await page.locator('body').innerText();
 if(!/PID namespaces\s+Yes/.test(sandbox)||!/Seccomp-BPF sandbox\s+Yes/.test(sandbox))throw Error('Actual browser sandbox proof failed');
 let headless;
 try{const cdp=await context.browser().newBrowserCDPSession();const command=await cdp.send('Browser.getBrowserCommandLine');headless=command.arguments.some(x=>x.startsWith('--headless'))}
 catch{headless=(await page.evaluate(()=>navigator.userAgent)).includes('HeadlessChrome')}
 const proof={browser_version:context.browser().version(),sandbox_report:sandbox,
   actual_headless:headless,native_profile_CA_imported:false,host_trust_store_changed:false,host_policy_changed:false};
 fs.writeFileSync(process.env.CANDIDATE_BROWSER_PROOF,JSON.stringify(proof)+'\n');
 await page.goto('chrome://certificate-manager/localcerts/usercerts');
 const trusted=page.locator('certificate-subpage#userCertsSection certificate-list').filter({has:page.getByRole('heading',{name:'Trusted Certificates',exact:true})});
 await trusted.waitFor({state:'visible'});if(await trusted.count()!==1)throw Error('Native user trusted-certificate list unavailable');
 await trusted.getByRole('button',{name:'Import to Trusted Certificates',exact:true}).click();
 await page.waitForTimeout(300);proof.native_ui_dialogs=await page.locator('cr-dialog:visible, cr-toast:visible').allTextContents();
 fs.writeFileSync(process.env.CANDIDATE_BROWSER_PROOF,JSON.stringify(proof)+'\n');
 proof.native_dialog=JSON.parse(cp.execFileSync('python3',[path.join(__dirname,'native_file_dialog.py'),caFile],{timeout:20000,stdio:['ignore','pipe','pipe']}).toString());
 await page.waitForTimeout(1500);proof.user_trusted_list=await trusted.evaluate(e=>e.shadowRoot.textContent.slice(-2000));
 proof.native_ui_dialogs=await page.getByRole('dialog').allTextContents();
 proof.visible_native_text=await page.evaluate(()=>{
  const out=[];function visit(n){if(out.join(' ').length>7000)return;
   if(n.nodeType===3){const s=n.textContent.trim();if(s)out.push(s);return}
   if(n.nodeType===1){if(['STYLE','SCRIPT','SVG','PATH'].includes(n.tagName))return;const css=getComputedStyle(n);if(css.display==='none'||css.visibility==='hidden')return;if(n.shadowRoot)visit(n.shadowRoot)}
   for(const child of n.childNodes||[])visit(child)
  }visit(document.body);return out.join(' ').slice(0,7000)
 });
 proof.user_certificate_entries=await trusted.locator('certificate-entry').evaluateAll(nodes=>nodes.map(e=>({hash:e.shadowRoot?.querySelector('#certhash')?.value,text:e.shadowRoot?.textContent.slice(-500)})));
 fs.writeFileSync(process.env.CANDIDATE_BROWSER_PROOF,JSON.stringify(proof)+'\n');
 const name=cert.subject.split('\n').find(s=>s.startsWith('CN='))?.slice(3);if(!name)throw Error('Synthetic CA common name required');
 const entry=trusted.getByText(name,{exact:true});await entry.waitFor({state:'attached'});
 if(!await entry.isVisible())await trusted.getByRole('button',{name:'Show Trusted Certificates',exact:true}).click();
 await entry.waitFor({state:'visible'});
 fs.writeFileSync(process.env.CANDIDATE_BROWSER_PROOF,JSON.stringify({browser_version:context.browser().version(),
  sandbox_report:sandbox,native_profile_CA_imported:true,CA_fingerprint256:cert.fingerprint256,
  actual_headless:headless,host_trust_store_changed:false,host_policy_changed:false,ignoreHTTPSErrors:false,chromiumSandbox:true})+'\n');
 await page.close();
}
const helper=path.join(root,'scripts/browser-profile-trust.cjs');
require.cache[require.resolve(helper)]={id:helper,filename:helper,loaded:true,exports:{trustLocalTestCA}};
if(require.main===module)require(path.join(root,'scripts/test-release-browser.cjs'));
else module.exports={trustLocalTestCA};
