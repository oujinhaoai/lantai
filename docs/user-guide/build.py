#!/usr/bin/env python3
"""Build this small, trusted Markdown manual with Python's standard library.

Supported subset: headings, paragraphs, flat lists, tables, fenced code, links,
strong and inline code. Raw HTML is always escaped. No plugins or remote assets.
"""
import argparse
import html
import json
import re
from pathlib import Path
from urllib.parse import quote

ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / "docs/user-guide"
OUTPUT = ROOT / "web/public/help"
REVISION = "v0.1.0-rc.4"
TOPICS = [
    ("README", "index", "从这里开始", "按任务找到入口，先认清操作位置和版本边界。"),
    ("install", "install", "在 NAS 安装", "镜像、项目配置、卷与首次启动。"),
    ("setup", "setup", "首次设置", "引导码、TOTP、恢复码及登记过期。"),
    ("accounts", "accounts", "账户与权限", "管理员、Agent、项目角色与会话。"),
    ("daily", "daily", "日常资源操作", "找项目、搜索、下载、入藏与续传。"),
    ("codex-mcp", "codex-mcp", "连接 Codex MCP", "本机配置、工作目录与连接检查。"),
    ("upgrade", "upgrade", "升级与保留数据", "保留实际卷身份，核对后恢复写入。"),
    ("backup", "backup", "备份与恢复", "离线备份、独立密钥与恢复对账。"),
    ("troubleshooting", "troubleshooting", "故障与版本兼容", "按错误定位，核对版本和支持范围。"),
]


def slug(value):
    value = re.sub(r"[`*]", "", value).strip().lower()
    return re.sub(r"[^\w\- ]", "", value).replace(" ", "-")


def link_url(url):
    if url.startswith(("https://", "#")):
        return url
    path, sep, anchor = url.partition("#")
    target = (SOURCE / path).resolve()
    if target.parent == SOURCE and target.suffix == ".md":
        page = next(item[1] for item in TOPICS if item[0] == target.stem)
        return page + ".html" + (sep + anchor if sep else "")
    relative = target.relative_to(ROOT).as_posix()
    return f"https://github.com/oujinhaoai/lantai/blob/{REVISION}/{quote(relative)}" + (sep + anchor if sep else "")


def inline(value):
    # Tokenize before escaping so code and URL text never become HTML.
    pattern = r"(`[^`]+`|\[[^\]]+\]\([^)]+\)|\*\*[^*]+\*\*)"
    result = []
    for token in re.split(pattern, value):
        if token.startswith("`") and token.endswith("`"):
            result.append("<code>" + html.escape(token[1:-1]) + "</code>")
        elif token.startswith("[") and "](" in token:
            label, url = token[1:-1].split("](", 1)
            result.append(f'<a href="{html.escape(link_url(url), quote=True)}">{html.escape(label)}</a>')
        elif token.startswith("**") and token.endswith("**"):
            result.append("<strong>" + html.escape(token[2:-2]) + "</strong>")
        else:
            result.append(html.escape(token))
    return "".join(result)


def render(markdown):
    lines = markdown.splitlines()
    result, headings = [], []
    i = 0
    while i < len(lines):
        line = lines[i].strip()
        if not line:
            i += 1
            continue
        if line.startswith("```"):
            language, code = line[3:], []
            i += 1
            while i < len(lines) and not lines[i].strip().startswith("```"):
                code.append(lines[i])
                i += 1
            if i == len(lines):
                raise ValueError("Unclosed code fence")
            result.append(f'<pre tabindex="0" aria-label="{html.escape(language or "text")} 示例"><code>' + html.escape("\n".join(code)) + "</code></pre>")
        elif re.match(r"^#{1,3} ", line):
            hashes, title = line.split(" ", 1)
            level, anchor = len(hashes), slug(title)
            result.append(f'<h{level} id="{anchor}">{inline(title)}</h{level}>')
            if level == 2:
                headings.append((anchor, title))
        elif line.startswith("|"):
            rows = []
            while i < len(lines) and lines[i].strip().startswith("|"):
                cells = [x.strip() for x in lines[i].strip().strip("|").split("|")]
                if not all(re.fullmatch(r":?-+:?", x) for x in cells):
                    rows.append(cells)
                i += 1
            header = "".join(f'<th scope="col">{inline(x)}</th>' for x in rows[0])
            body = "".join("<tr>" + "".join(f"<td>{inline(x)}</td>" for x in row) + "</tr>" for row in rows[1:])
            result.append('<div class="table-scroll" role="region" aria-label="对照表，可横向滚动" tabindex="0"><table><thead><tr>' + header + "</tr></thead><tbody>" + body + "</tbody></table></div>")
            continue
        elif re.match(r"^(\d+\. |- )", line):
            ordered = bool(re.match(r"^\d+\. ", line))
            tag, items = ("ol" if ordered else "ul"), []
            matcher = r"^\d+\. " if ordered else r"^- "
            while i < len(lines) and re.match(matcher, lines[i].strip()):
                items.append("<li>" + inline(re.sub(matcher, "", lines[i].strip())) + "</li>")
                i += 1
            result.append(f"<{tag}>" + "".join(items) + f"</{tag}>")
            continue
        else:
            paragraph = [line]
            while i + 1 < len(lines) and lines[i + 1].strip() and not re.match(r"^(#|```|\||- |\d+\. )", lines[i + 1].strip()):
                i += 1
                paragraph.append(lines[i].strip())
            result.append("<p>" + inline(" ".join(paragraph)) + "</p>")
        i += 1
    return "\n".join(result), headings


def navigation(current):
    result = []
    for n, (_, page, title, _) in enumerate(TOPICS):
        active = 'aria-current="page" ' if page == current else ""
        result.append(f'<a {active}href="{page}.html"><span class="nav-number">{n:02}</span>{title}</a>')
    return "".join(result)


def build():
    files, search = {}, []
    for n, (source, page, title, summary) in enumerate(TOPICS):
        raw = (SOURCE / (source + ".md")).read_text()
        content, headings = render(raw)
        search.append({"title": title, "url": page + ".html", "text": raw, "summary": summary})
        for anchor, heading in headings:
            start = raw.index("## " + heading)
            end = raw.find("\n## ", start + 3)
            search.append({"title": title + " · " + heading, "url": page + ".html#" + anchor, "text": raw[start:end if end != -1 else len(raw)], "summary": summary})
        nav = navigation(page)
        toc = "".join(f'<li><a href="#{anchor}">{html.escape(label)}</a></li>' for anchor, label in headings)
        cards = ""
        if n == 0:
            cards = '<section class="cards" aria-label="按任务阅读">' + "".join(f'<a class="card" href="{p}.html"><span class="eyebrow">任务 {j:02}</span><h2>{t}</h2><p>{s}</p><span aria-hidden="true">阅读指南 →</span></a>' for j, (_, p, t, s) in enumerate(TOPICS[1:], 1)) + "</section>"
            content = re.sub(r'(<h2 id="选择任务">.*?</h2>)\s*<div class="table-scroll".*?</div>', r'\1' + cards, content, count=1, flags=re.S)
        else:
            content = content.replace('</p>', '</p><details class="toc"><summary>本页目录</summary><ul>' + toc + '</ul></details>', 1)
        previous = TOPICS[n - 1] if n else None
        following = TOPICS[n + 1] if n < len(TOPICS) - 1 else None
        pagination = (f'<a href="{previous[1]}.html">← {previous[2]}</a>' if previous else '<span></span>') + (f'<a href="{following[1]}.html">{following[2]} →</a>' if following else '<a href="index.html">返回手册首页 ↑</a>')
        files[page + ".html"] = f'''<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="referrer" content="no-referrer"><meta name="description" content="{summary}"><title>{title} · Lantai 帮助</title><link rel="icon" type="image/svg+xml" href="icon.svg"><link rel="stylesheet" href="help.css"><script src="search-data.js" defer></script><script src="help.js" defer></script></head>
<body><a class="skip" href="#content">跳到正文</a>
<header class="topbar"><a class="brand" href="index.html"><span class="brand-mark" aria-hidden="true">兰</span><span>LANTAI <small>使用帮助</small></span></a><div class="topbar-actions"><span class="edition">简体中文 · rc.4</span><a class="app-back" href="/"><span aria-hidden="true">←</span> 返回兰台</a></div></header>
<div class="layout"><aside class="sidebar"><p class="eyebrow">用户手册</p><nav aria-label="手册目录">{nav}</nav><p class="sidebar-note">从操作位置到成功标志。<br>一步一步，完成手头的事。</p><a class="source" href="https://github.com/oujinhaoai/lantai/tree/{REVISION}">查看对应版本源码 ↗</a></aside>
<div class="body-column"><details class="mobile-nav"><summary>手册目录 · {title}</summary><nav aria-label="移动端手册目录">{nav}</nav></details>
<div class="search" role="search" hidden><label for="help-search">搜索全部帮助</label><div class="search-row"><input id="help-search" type="search" maxlength="120" placeholder="试试：恢复码、备份、项目权限" aria-controls="search-results" autocomplete="off"><button type="button" id="search-clear">清除</button></div><p class="search-hint">仅搜索通用手册，不会访问你的项目或发送输入。</p><section id="search-results" aria-label="搜索结果" hidden><p id="search-status" role="status" aria-live="polite"></p><ul id="search-list"></ul></section></div>
<main id="content" tabindex="-1"><div class="page-meta"><span class="eyebrow">{("开始使用" if n == 0 else f"任务 {n:02}")}</span><span>文档核对 · 2026-10-13</span></div>
<article>{content}</article>
<nav class="pagination" aria-label="上一篇与下一篇">{pagination}</nav>
<footer>对应 v0.1.0-rc.4 统一交付包。发行状态以 Release 为准；文档版本不代表当前服务器版本。<br>所有示例均为合成数据。帮助页无需登录。</footer></main></div></div></body></html>
'''
    files["search-data.js"] = "// Generated by docs/user-guide/build.py; public manual content only.\nwindow.LANTAI_HELP = " + json.dumps(search, ensure_ascii=False, separators=(",", ":")) + ";\n"
    return files


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="Fail on generated content drift")
    args = parser.parse_args()
    files = build()
    if args.check:
        wrong = [name for name, data in files.items() if not (OUTPUT / name).exists() or (OUTPUT / name).read_text() != data]
        if wrong:
            raise SystemExit("Help output drift: " + ", ".join(wrong))
        print(f"Help output current: {len(files)} files")
    else:
        OUTPUT.mkdir(parents=True, exist_ok=True)
        for name, data in files.items():
            (OUTPUT / name).write_text(data)
        print(f"Built {len(files)} help files")


if __name__ == "__main__":
    main()
