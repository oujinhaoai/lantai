import { useMemo, useState, type ReactNode } from "react";
import Markdown from "react-markdown";
import { common, createLowlight } from "lowlight";
import type { RootContent } from "hast";
const lowlight = createLowlight(common);
const languages: Record<string, string> = {
  py: "python",
  go: "go",
  js: "javascript",
  jsx: "javascript",
  ts: "typescript",
  tsx: "typescript",
  json: "json",
  yaml: "yaml",
  yml: "yaml",
  toml: "ini",
  ini: "ini",
  conf: "ini",
  html: "xml",
  htm: "xml",
  svg: "xml",
  xml: "xml",
  css: "css",
  sh: "bash",
  bash: "bash",
  sql: "sql",
  rs: "rust",
  java: "java",
  c: "c",
  h: "c",
  cpp: "cpp",
  md: "markdown",
  markdown: "markdown",
};
function highlighted(text: string, language: string | undefined) {
  const lines: ReactNode[][] = [[]];
  let key = 0;
  const add = (value: string, className: string) =>
    value.split("\n").forEach((part, index) => {
      if (index) lines.push([]);
      if (part)
        lines.at(-1)!.push(
          <span key={key++} className={className || undefined}>
            {part}
          </span>,
        );
    });
  const visit = (node: RootContent, classes = "") => {
    if (node.type === "text") add(node.value, classes);
    else if (node.type === "element") {
      const next = [
        classes,
        ...(Array.isArray(node.properties.className)
          ? node.properties.className
          : []),
      ].join(" ");
      node.children.forEach((child) => visit(child, next));
    }
  };
  try {
    if (text.length <= 65536 && language && lowlight.registered(language))
      lowlight
        .highlight(language, text)
        .children.forEach((node) => visit(node));
    else add(text, "");
  } catch {
    add(text, "");
  }
  return lines;
}
export default function TextPreview({
  text,
  path,
  markdown,
  truncated,
}: {
  text: string;
  path: string;
  markdown: boolean;
  truncated: boolean;
}) {
  const [source, setSource] = useState(!markdown);
  const [copy, setCopy] = useState("");
  const rawLines = text.split("\n");
  const shown = rawLines.slice(0, 2000).join("\n");
  const language = languages[path.split(".").pop()?.toLowerCase() || ""];
  const lines = useMemo(() => highlighted(shown, language), [shown, language]);
  const copyText = async () => {
    try {
      await navigator.clipboard.writeText(shown);
      setCopy("已复制当前预览文本");
    } catch {
      setCopy("浏览器未允许复制，请选择源码文本复制。");
    }
  };
  return (
    <div className="text-preview">
      <div className="preview-actions">
        {markdown && (
          <button
            className="secondary"
            aria-pressed={source}
            onClick={() => setSource(!source)}
          >
            {source ? "阅读 Markdown" : "查看源码"}
          </button>
        )}
        <button className="secondary" onClick={() => void copyText()}>
          复制当前预览
        </button>
        <span role="status">{copy}</span>
      </div>
      {!source && markdown ? (
        <div className="markdown-preview">
          <Markdown
            skipHtml
            components={{
              img: ({ alt }) => <span>［图片未加载：{alt || "无说明"}］</span>,
              a: ({ children }) => (
                <span className="markdown-link">{children}</span>
              ),
            }}
          >
            {shown}
          </Markdown>
          <p className="subtle">
            Markdown 以安全只读方式显示：不执行
            HTML，不加载图片或外部资源，链接仅显示文字。
          </p>
        </div>
      ) : (
        <pre
          className="code-preview"
          tabIndex={0}
          aria-label="带行号的文本源码"
        >
          <code>
            {lines.map((line, index) => (
              <span className="code-line" key={index}>
                <span className="line-number" aria-hidden="true">
                  {index + 1}
                </span>
                <span>{line.length ? line : " "}</span>
              </span>
            ))}
          </code>
        </pre>
      )}
      {(truncated || rawLines.length > 2000) && (
        <p className="subtle">
          预览已截断{rawLines.length > 2000 ? "（最多显示 2000 行）" : ""}
          ；复制仅包含当前显示的文本。
        </p>
      )}
      <p className="subtle">
        源码区可单独横向滚动；复制仅包含当前预览显示的文本。
      </p>
      <p className="subtle">
        只读 UTF-8 文本，不编辑或运行代码。HTML、SVG 等仅显示源码。
      </p>
    </div>
  );
}
