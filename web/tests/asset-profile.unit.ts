import assert from "node:assert/strict";
import test from "node:test";
import {
  emptyProfile,
  normalizeProfile,
  profileDraft,
  profileFromDraft,
} from "../src/asset-profile";
import { metadataPatch, restoreMetadataDraft } from "../src/asset-tools";
test("structured profile mirrors all eight Go fields, NFC vocabulary and inert paths", () => {
  const p = profileFromDraft({
    ...emptyProfile(),
    characters: "e\u0301\nhero-a\nhero-a",
    source: "synthetic",
    business_state: "pending_review",
    legacy_state: "awaiting-human-review",
    legacy_version: "v007",
    source_paths: "/synthetic/path,with-comma\nrelative/source",
    notes: "preserve original note\nsecond note",
  });
  assert.equal(p.schema, "lantai.asset-profile/v1");
  assert.deepEqual(p.characters, ["hero-a", "é"]);
  assert.deepEqual(p.source_paths, [
    "/synthetic/path,with-comma",
    "relative/source",
  ]);
  assert.equal(p.legacy_state, "awaiting-human-review");
  assert.equal(p.notes.length, 2);
  assert.deepEqual(profileFromDraft(profileDraft(p)), p);
});
test("profile edits preserve unknown extra and ordinary tags; do not introduce rights", () => {
  const extra = {
    custom: { counter: "9007199254740993", flag: true },
    asset_profile: normalizeProfile({
      source: "old",
      business_state: "rejected",
    }),
  };
  const before = JSON.stringify(extra);
  const patch = metadataPatch({
    title: "Synthetic",
    summary: "",
    tags: "freeform\nsource:old\nstate:rejected",
    subjects: "scene-a",
    profile: {
      ...emptyProfile(),
      characters: "hero-a",
      source: "new",
      business_state: "active",
    },
  });
  assert.deepEqual(patch.tags, [
    "character:hero-a",
    "freeform",
    "source:new",
    "state:active",
  ]);
  assert.deepEqual(patch.subjects, ["hero-a", "scene-a"]);
  assert.equal("extra" in patch, false);
  assert.equal(patch.profile?.source, "new");
  assert.equal(JSON.stringify(extra), before);
  assert.equal("rights" in patch, false);
  assert.equal("sensitivity" in patch, false);
});
test("profile rejects security fields, false approval and bad Unicode", () => {
  for (const p of [
    { rights: {} },
    { noai: false },
    { business_state: "approved" },
    { notes: ["bad\u202evalue"] },
    { source: "bad\ud800" },
    { schema: "lantai.asset-profile/v99" },
    { characters: ["x".repeat(55)] },
  ])
    assert.throws(() => normalizeProfile(p));
});
test("uncertain structured metadata draft retains profile, revision and original key", () => {
  const draft = {
    title: "Synthetic",
    summary: "",
    tags: "freeform",
    subjects: "",
    profile: { ...emptyProfile(), source: "synthetic" },
  };
  const value = {
    schema: "lantai.web-metadata-draft/v1",
    origin: "https://synthetic.invalid",
    asset_id: "01J0000000000000000000000A",
    draft,
    attempt: {
      key: "original-profile-intent",
      revision: 9,
      body: metadataPatch(draft),
    },
    uncertain: true,
  };
  const restored = restoreMetadataDraft(value, value.origin, value.asset_id);
  assert.deepEqual(restored.attempt, value.attempt);
  assert.equal(restored.uncertain, true);
  assert.throws(
    () =>
      restoreMetadataDraft(
        {
          ...value,
          draft: { ...draft, profile: { ...draft.profile, source: "changed" } },
        },
        value.origin,
        value.asset_id,
      ),
    /冻结内容/,
  );
});
