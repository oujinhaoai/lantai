// The route is a locator, not a grant. The caller must still resolve the
// exact path against an authorized frozen manifest.
const identifier = /^[0-7][0-9A-HJKMNP-TV-Z]{25}$/;

function portableRelative(path: string) {
  return (
    !path.includes("\\") &&
    !Array.from(path).some(
      (char) => char.charCodeAt(0) < 32 || char.charCodeAt(0) === 127,
    ) &&
    path
      .split("/")
      .every((part) => part !== "" && part !== "." && part !== "..")
  );
}

export function fixedFileSelection(rawQuery: string): {
  valid: boolean;
  path?: string;
} {
  try {
    const matches = rawQuery.split("&").flatMap((pair) => {
      const cut = pair.indexOf("=");
      if (
        decodeURIComponent(
          (cut < 0 ? pair : pair.slice(0, cut)).replace(/\+/g, " "),
        ) !== "file"
      )
        return [];
      return [
        decodeURIComponent(
          (cut < 0 ? "" : pair.slice(cut + 1)).replace(/\+/g, " "),
        ),
      ];
    });
    if (!matches.length) return { valid: true };
    if (matches.length !== 1 || !portableRelative(matches[0]))
      return { valid: false };
    return { valid: true, path: matches[0] };
  } catch {
    return { valid: false };
  }
}

export function fixedFileURL(
  href: string,
  asset: string,
  version: string,
  path: string,
) {
  const url = new URL(href);
  if (
    url.protocol !== "https:" ||
    url.username ||
    url.password ||
    !identifier.test(asset) ||
    !identifier.test(version) ||
    !portableRelative(path)
  )
    return undefined;
  url.search = "";
  url.hash = `/assets/${asset}?version=${version}&${new URLSearchParams({ file: path })}`;
  return url.toString();
}
