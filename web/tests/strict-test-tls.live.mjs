/* global process, console */
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createServer } from "node:https";
import { connect } from "node:tls";
import { X509Certificate } from "node:crypto";

// This is a transport test only. It does not stand in for a browser run or
// device execution, and never changes process-wide/host certificate trust.
const ca = await readFile(process.env.LANTAI_WEB_CA_FILE);
const certificate = await readFile(process.env.LANTAI_WEB_TLS_CERT_FILE);
const key = await readFile(process.env.LANTAI_WEB_TLS_KEY_FILE);
const server = createServer(
  { cert: certificate, key, minVersion: "TLSv1.2" },
  (_request, response) => response.end("synthetic TLS fixture\n"),
);
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
try {
  const address = server.address();
  const handshake = (extra) =>
    new Promise((resolve, reject) => {
      const socket = connect({
        host: "127.0.0.1",
        port: address.port,
        servername: "localhost",
        rejectUnauthorized: true,
        ...extra,
      });
      socket.setTimeout(5000, () =>
        socket.destroy(new Error("synthetic TLS timeout")),
      );
      socket.once("secureConnect", () => {
        const result = {
          authorized: socket.authorized,
          protocol: socket.getProtocol(),
        };
        socket.end();
        resolve(result);
      });
      socket.once("error", reject);
    });
  const valid = await handshake({ ca });
  assert.equal(valid.authorized, true);
  assert.ok(["TLSv1.2", "TLSv1.3"].includes(valid.protocol));
  await assert.rejects(handshake({ ca: [] }), (error) =>
    [
      "UNABLE_TO_VERIFY_LEAF_SIGNATURE",
      "SELF_SIGNED_CERT_IN_CHAIN",
      "DEPTH_ZERO_SELF_SIGNED_CERT",
    ].includes(error.code),
  );
  await assert.rejects(
    handshake({ ca, servername: "wrong-host.synthetic.invalid" }),
    (error) => error.code === "ERR_TLS_CERT_ALTNAME_INVALID",
  );
  console.log(
    JSON.stringify({
      schema: "lantai.strict-synthetic-tls-evidence/v1",
      passed: [
        "explicit local CA + localhost",
        "untrusted CA rejected",
        "wrong hostname rejected",
      ],
      certificate_sha256: new X509Certificate(certificate).fingerprint256,
      protocol: valid.protocol,
      browser_executed: false,
    }),
  );
} finally {
  await new Promise((resolve) => server.close(resolve));
}
