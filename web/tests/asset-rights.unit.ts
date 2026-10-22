import assert from "node:assert/strict";
import test from "node:test";
import {
  emptyRightsDraft,
  rightsAssertion,
  prepareRightsApproval,
  rightsEvidence,
  rightsTarget,
  type RightsSnapshot,
} from "../src/asset-rights";
const instance = "01J0000000000000000000000A",
  asset = "01J0000000000000000000000B",
  version = "01J0000000000000000000000C",
  evidence = "01J0000000000000000000000D";
const snapshot: RightsSnapshot = {
  subject: { instance_id: instance, asset_id: asset, version_id: version },
  manifest_digest: `sha256:${"a".repeat(64)}`,
  revision: 3,
  last_assertion_id: "01J0000000000000000000000E",
  rights: {
    usage: "reference",
    license: "LicenseRef-Synthetic",
    sensitivity: "normal",
    noai: true,
    redistribute_raw: false,
  },
  restricted_source: false,
  deny_purposes: [],
  evidence_revision: 1,
  evidence: [{ record_id: evidence, revision: 1 }],
  purposes: [],
};
test("rights request binds exact version, original assertion revision, accepted evidence and explicit fields", () => {
  const request = rightsAssertion(snapshot, {
    ...emptyRightsDraft(),
    kind: "release",
    noai: "false",
    evidence_ids: evidence,
    reason: "new accepted source evidence",
  });
  assert.equal(request.expected_revision, 3);
  assert.equal(request.replaces_assertion_id, snapshot.last_assertion_id);
  assert.deepEqual(request.fields, { noai: false });
  assert.deepEqual(request.subject, snapshot.subject);
  assert.equal("rights" in request, false);
});
test("rights evidence stays unknown and never resolves or fetches a descriptive source path", () => {
  const request = rightsEvidence(
    snapshot,
    "synthetic evidence",
    "https://synthetic.invalid/input\n/synthetic/inert-path",
  );
  assert.deepEqual(request.evidence, {
    note: "synthetic evidence",
    external_inputs: [
      {
        source_url: "https://synthetic.invalid/input",
        rights_status: "unknown",
      },
      { source_url: "/synthetic/inert-path", rights_status: "unknown" },
    ],
  });
  assert.equal(request.expected_revision, 1);
});
test("assertion adapters reject foreign evidence, malformed uses and fields in evidence-only index", () => {
  assert.throws(
    () =>
      rightsAssertion(snapshot, {
        ...emptyRightsDraft(),
        evidence_ids: instance,
        reason: "wrong evidence",
      }),
    /已接受/,
  );
  assert.throws(
    () =>
      rightsAssertion(snapshot, {
        ...emptyRightsDraft(),
        kind: "evidence",
        noai: "false",
        evidence_ids: evidence,
        reason: "index only",
      }),
    /不能更改/,
  );
  assert.throws(
    () =>
      rightsAssertion(snapshot, {
        ...emptyRightsDraft(),
        uses: '[{"path":"/tmp/script"}]',
        evidence_ids: evidence,
        reason: "bad relation",
      }),
    /固定引用/,
  );
  assert.throws(
    () =>
      rightsTarget({
        asset_id: asset,
        version_id: version,
        project_id: instance,
        version_number: 1,
        committed_at: "",
        manifest_digest: snapshot.manifest_digest,
        uri: `lantai://${instance}/assets/${asset}/versions/${evidence}`,
      }),
    /固定版本/,
  );
});

test("human rights adapter refuses changed server resource/project/request before requesting OTP", async () => {
  const draft = {
    ...emptyRightsDraft(),
    kind: "release" as const,
    noai: "false",
    evidence_ids: evidence,
    reason: "new precise evidence",
  };
  const body = rightsAssertion(snapshot, draft);
  const challenge = {
    challenge_id: "01J0000000000000000000000F",
    operation_id: "01J0000000000000000000000G",
    action: "provenance.release_restriction",
    summary: "Complete synthetic target",
    expires_at: "2099-01-01T00:00:00Z",
    targets: [{ kind: "domain_target", id: version, expected_revision: 3 }],
  };
  const action = {
    action: challenge.action,
    project_id: instance,
    resource_id: version,
    resource_revision: 3,
    manifest_digest: snapshot.manifest_digest,
    request: body,
  };
  const calls: string[] = [];
  const transport = async <T>(path: string) => {
    calls.push(path);
    return { challenge, items: [action] } as T;
  };
  const good = await prepareRightsApproval(
    snapshot,
    draft,
    instance,
    transport,
  );
  assert.equal(good.actions?.[0].resource_id, version);
  for (const altered of [
    { ...action, resource_id: asset },
    { ...action, project_id: asset },
    { ...action, request: { ...body, expected_revision: 2 } },
  ])
    await assert.rejects(
      prepareRightsApproval(
        snapshot,
        draft,
        instance,
        async <T>() => ({ challenge, items: [altered] }) as T,
      ),
      /未请求动态码/,
    );
  assert.ok(calls.every((path) => path === "/api/v1/human/prepare"));
});
