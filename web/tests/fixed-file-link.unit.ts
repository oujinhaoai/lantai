import { strict as assert } from "node:assert";
import { test } from "node:test";
import { fixedFileSelection, fixedFileURL } from "../src/fixed-file-link";

const asset = "01M80QVZBZM6Z03R52YT0RCPZC";
const version = "01M80QVZBZKGD0G52Z5GVZAMD9";

test("copies an HTTPS locator with exact Unicode and encoded second-layer delimiters only", () => {
  const file = "镜头/角色 1+a%?#.png";
  const href = fixedFileURL(
    "https://demo.example:8443/?unsafe=old#/wrong",
    asset,
    version,
    file,
  );
  assert.ok(href);
  const url = new URL(href);
  assert.equal(url.origin, "https://demo.example:8443");
  assert.equal(url.search, "");
  assert.ok(url.hash.startsWith(`#/assets/${asset}?version=${version}&file=`));
  assert.deepEqual(fixedFileSelection(url.hash.split("?")[1]), {
    valid: true,
    path: file,
  });
  assert.notEqual(
    fixedFileURL(href, asset, version, "另一个/角色 1+a%?#.png"),
    href,
  );
  assert.equal(
    fixedFileURL("http://localhost:8000/", asset, version, file),
    undefined,
  );
});

test("bad or ambiguous file selectors fail without a default file", () => {
  for (const raw of [
    "file",
    "file=",
    "file=a&%66ile=b",
    "file=%ZZ",
    "file=%ED%A0%80",
    "file=a%2F..%2Fb",
    "file=%2Fa",
    "file=a%5Cb",
    "file=a%00b",
  ])
    assert.deepEqual(fixedFileSelection(raw), { valid: false }, raw);
  assert.deepEqual(fixedFileSelection("version=else"), { valid: true });
});
