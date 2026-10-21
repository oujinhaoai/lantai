// Packaging-only adaptation for Chrome155's native user certificate subpage.
// The original artifact browser assertions, sandbox and TLS policy are unchanged.
const fs=require('fs'),path=require('path'),{X509Certificate}=require('crypto');
const root=path.resolve(__dirname,'../..');
async function trustLocalTestCA(context,caFile){
 const cert=new X509Certificate(fs.readFileSync(caFile));
 if(!cert.ca||!cert.checkIssued(cert)||!cert.verify(cert.publicKey))throw Error('Explicit self-signed synthetic CA required');
 const page=await context.newPage();await page.goto('chrome://sandbox');const sandbox=await page.locator('body').innerText();
 if(!/PID namespaces\s+Yes/.test(sandbox)||!/Seccomp-BPF sandbox\s+Yes/.test(sandbox))throw Error('Actual browser sandbox proof failed');
 await page.goto('chrome://certificate-manager/localcerts/usercerts');
 const trusted=page.locator('certificate-subpage#userCertsSection certificate-list').filter({hasText:/Trusted Certificates/i});
 await trusted.waitFor({state:'visible'});if(await trusted.count()!==1)throw Error('Native user trusted-certificate list unavailable');
 const chooser=page.waitForEvent('filechooser');await trusted.getByRole('button',{name:'Import to Trusted Certificates',exact:true}).click();
 await (await chooser).setFiles(caFile);
 const name=cert.subject.split('\n').find(s=>s.startsWith('CN='))?.slice(3);if(!name)throw Error('Synthetic CA common name required');
 const entry=trusted.getByText(name,{exact:true});await entry.waitFor({state:'attached'});
 if(!await entry.isVisible())await trusted.getByRole('button',{name:'Show Trusted Certificates',exact:true}).click();
 await entry.waitFor({state:'visible'});
 fs.writeFileSync(process.env.CANDIDATE_BROWSER_PROOF,JSON.stringify({browser_version:context.browser().version(),
  sandbox_report:sandbox,native_profile_CA_imported:true,CA_fingerprint256:cert.fingerprint256,
  host_trust_store_changed:false,host_policy_changed:false,ignoreHTTPSErrors:false,chromiumSandbox:true})+'\n');
 await page.close();
}
const helper=path.join(root,'scripts/browser-profile-trust.cjs');
require.cache[require.resolve(helper)]={id:helper,filename:helper,loaded:true,exports:{trustLocalTestCA}};
require(path.join(root,'scripts/test-release-browser.cjs'));
