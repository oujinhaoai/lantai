// Trust only an explicitly selected synthetic CA through Chromium's native
// profile certificate manager. No host store, certificate-error override, or
// fallback launch mode is available here.
const { readFile } = require("node:fs/promises");
const { X509Certificate } = require("node:crypto");

async function trustLocalTestCA(context, caFile) {
  const certificate = new X509Certificate(await readFile(caFile));
  if (
    !certificate.ca ||
    !certificate.checkIssued(certificate) ||
    !certificate.verify(certificate.publicKey)
  ) {
    throw new Error("The selected test certificate must be a self-signed CA.");
  }
  const page = context.pages()[0] || (await context.newPage());
  await page.goto("chrome://certificate-manager/localcerts");
  const trusted = page
    .locator("certificate-list-v2")
    .filter({ hasText: /Trusted certificates/i });
  await trusted.waitFor({ state: "visible" });
  if ((await trusted.count()) !== 1)
    throw new Error(
      "Native profile-scoped trusted certificates list is unavailable.",
    );
  const chooser = page.waitForEvent("filechooser");
  await trusted.getByRole("button", { name: /^Import$/i }).click();
  await (await chooser).setFiles(caFile);
  const commonName = certificate.subject
    .split("\n")
    .find((value) => value.startsWith("CN="))
    ?.slice(3);
  if (!commonName) throw new Error("The synthetic CA must have a common name.");
  await trusted
    .getByText(commonName, { exact: true })
    .waitFor({ state: "visible" });
  await page.close();
}
module.exports = { trustLocalTestCA };
