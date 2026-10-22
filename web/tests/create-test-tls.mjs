/* global process, console */
import { mkdir, writeFile, chmod } from "node:fs/promises";
import { resolve, join } from "node:path";
import { execFileSync } from "node:child_process";

// Explicit output only: never install CA material into the operating system.
const destination = process.argv[2];
if (!destination) throw new Error("Pass a new private test TLS directory.");
const directory = resolve(destination);
await mkdir(directory, { mode: 0o700 }); // Refuse replacing an existing directory.
const ca = join(directory, "ca.pem");
const cert = join(directory, "localhost.pem");
const key = join(directory, "localhost-key.pem");
const caKey = join(directory, "ca-key.pem");
const request = join(directory, "localhost.csr");
const extensions = join(directory, "localhost.ext");
await writeFile(
  extensions,
  "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:localhost,IP:127.0.0.1\n",
  { mode: 0o600 },
);
const run = (args) =>
  execFileSync("openssl", args, { stdio: ["ignore", "ignore", "pipe"] });
run([
  "req",
  "-x509",
  "-newkey",
  "rsa:2048",
  "-nodes",
  "-sha256",
  "-days",
  "7",
  "-subj",
  "/CN=Lantai Synthetic Browser Test CA",
  "-addext",
  "basicConstraints=critical,CA:TRUE",
  "-addext",
  "keyUsage=critical,keyCertSign,cRLSign",
  "-keyout",
  caKey,
  "-out",
  ca,
]);
run([
  "req",
  "-newkey",
  "rsa:2048",
  "-nodes",
  "-sha256",
  "-subj",
  "/CN=localhost",
  "-keyout",
  key,
  "-out",
  request,
]);
run([
  "x509",
  "-req",
  "-in",
  request,
  "-CA",
  ca,
  "-CAkey",
  caKey,
  "-CAcreateserial",
  "-days",
  "3",
  "-sha256",
  "-extfile",
  extensions,
  "-out",
  cert,
]);
for (const file of [
  ca,
  cert,
  key,
  caKey,
  request,
  extensions,
  join(directory, "ca.srl"),
])
  await chmod(file, 0o600);
console.log(
  JSON.stringify(
    {
      LANTAI_WEB_CA_FILE: ca,
      NODE_EXTRA_CA_CERTS: ca,
      LANTAI_WEB_TLS_CERT_FILE: cert,
      LANTAI_WEB_TLS_KEY_FILE: key,
    },
    null,
    2,
  ),
);
