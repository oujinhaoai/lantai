#!/usr/bin/env python3
"""Check generated help, local links, CSP-safe markup and documented CLI flags.

Optionally pass --binary PATH to check documented command help without making
network calls. This check reads source only and uses no deployment credentials.
"""
import argparse
from html.parser import HTMLParser
import re
import shlex
import subprocess
from urllib.parse import unquote, urlsplit

import build


class Page(HTMLParser):
    def __init__(self, text):
        super().__init__()
        self.ids, self.links, self.h1 = set(), [], 0
        self.app_returns = 0
        self.inline_script = False
        self.feed(text)

    def handle_starttag(self, tag, attributes):
        attrs = dict(attributes)
        if "id" in attrs:
            assert attrs["id"] not in self.ids, f"duplicate id: {attrs['id']}"
            self.ids.add(attrs["id"])
        if tag == "h1":
            self.h1 += 1
        if "app-back" in attrs.get("class", "").split():
            assert tag == "a" and attrs.get("href") == "/" and "target" not in attrs, "invalid application return"
            self.app_returns += 1
        assert not any(name.startswith("on") or name == "style" for name in attrs), "inline handler/style"
        assert tag != "style", "inline style"
        assert tag != "script" or "src" in attrs, "inline script"
        for name in ("href", "src"):
            if name in attrs:
                self.links.append(attrs[name])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary")
    args = parser.parse_args()
    expected = build.build()
    for name, content in expected.items():
        assert (build.OUTPUT / name).read_text() == content, f"generated drift: {name}"
    pages = {name: Page(content) for name, content in expected.items() if name.endswith(".html")}
    count = 0
    for name, page in pages.items():
        assert page.h1 == 1, f"{name}: expected one h1"
        assert page.app_returns == 1 and page.links.count("/") == 1, f"{name}: expected one application return"
        for href in page.links:
            if href == "/":
                continue  # The sole root link is the native, same-origin application return.
            parsed = urlsplit(href)
            if parsed.scheme:
                assert parsed.scheme == "https", f"unsafe URL: {href}"
                continue
            assert not parsed.netloc and not href.startswith("/"), f"nonrelative asset: {href}"
            target = parsed.path or name
            assert (build.OUTPUT / target).is_file(), f"{name}: missing {target}"
            if parsed.fragment:
                assert unquote(parsed.fragment) in pages[target].ids, f"{name}: missing anchor {href}"
            count += 1

    all_go = "\n".join(p.read_text() for directory in ("cmd/lantai", "internal/cli") for p in (build.ROOT / directory).glob("*.go") if not p.name.endswith("_test.go"))
    flags = set(re.findall(r'\.(?:String|StringVar|Bool|BoolVar|Int|IntVar|Int64|Duration)\([^\n]*?"([a-z][a-z0-9-]+)"', all_go))
    commands = set()
    md_links = 0
    for source, _, _, _ in build.TOPICS:
        text = (build.SOURCE / (source + ".md")).read_text()
        assert not re.search(r"/Users/haoge|/Volumes/990EVO|192\.168\.31\.157|lantai\.haogeaidev\.com|(?:ltk|lts)_[A-Za-z0-9]{16}", text), f"private data in {source}"
        for label, href in re.findall(r"\[([^\]]+)\]\(([^)]+)\)", text):
            if href.startswith(("https://", "#")):
                continue
            target, _, anchor = href.partition("#")
            path = (build.SOURCE / target).resolve()
            assert path.is_file(), f"{source}: missing source link {label} -> {href}"
            if anchor:
                headings = [build.slug(t) for t in re.findall(r"^#{1,6} (.+)$", path.read_text(), re.M)]
                assert unquote(anchor) in headings, f"{source}: missing source anchor {href}"
            md_links += 1
        for shell in re.findall(r"```sh\n(.*?)\n```", text, re.S):
            subprocess.run(["sh", "-n"], input=shell, text=True, check=True)
            # Parse logical shell lines; continued docker compose examples are
            # valid shell and must not fail on a trailing backslash.
            for line in shell.replace("\\\n", " ").splitlines():
                parts = shlex.split(line)
                if not parts or parts[0] != "lantai":
                    continue
                command = [parts[1]]
                if len(parts) > 2 and not parts[2].startswith("-"):
                    command.append(parts[2])
                commands.add(tuple(command))
                for part in parts[2:]:
                    if part.startswith("-"):
                        assert part.lstrip("-").split("=", 1)[0] in flags, f"unknown CLI flag: {part}"
    if args.binary:
        for command in sorted(commands):
            result = subprocess.run([args.binary, *command, "--help"], capture_output=True, text=True, timeout=15)
            help_text = result.stdout + result.stderr
            # Older native commands report flag.ErrHelp as exitUsage (2).
            assert result.returncode in (0, 2) and ("Usage" in help_text or "远程命令" in help_text), f"help failed: {command}: {help_text}"
            assert "flag provided but not defined" not in help_text and "unknown subcommand" not in help_text, f"unknown command: {command}"
    # The renderer must never pass Markdown's HTML or script URLs through.
    assert "&lt;script&gt;" in build.inline("<script>")
    try:
        build.link_url("javascript:alert(1)")
    except (ValueError, StopIteration):
        pass
    else:
        # Non-HTTPS input is converted to a fixed repository URL, never executed.
        assert build.link_url("javascript:alert(1)").startswith("https://github.com/")
    print(f"PASS: {len(pages)} pages, {count} local HTML links, {len(pages)} application returns, {md_links} source links, {len(commands)} CLI command forms; generated/CSP/privacy/shell checks")


if __name__ == "__main__":
    main()
