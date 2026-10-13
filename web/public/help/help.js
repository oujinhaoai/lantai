"use strict";

// Progressive enhancement only; navigation and every article work without JS.
(() => {
  const search = document.querySelector(".search");
  const input = document.querySelector("#help-search");
  const results = document.querySelector("#search-results");
  const status = document.querySelector("#search-status");
  const list = document.querySelector("#search-list");
  const clear = document.querySelector("#search-clear");
  if (!Array.isArray(window.LANTAI_HELP)) return;
  search.hidden = false;
  function update() {
    const query = input.value.trim().toLocaleLowerCase();
    list.replaceChildren();
    results.hidden = !query;
    if (!query) return;
    const terms = query.split(/\s+/);
    const matches = window.LANTAI_HELP.filter((entry) =>
      terms.every((term) => `${entry.title} ${entry.text}`.toLocaleLowerCase().includes(term)),
    );
    status.textContent = matches.length
      ? `找到 ${matches.length} 处内容，显示前 ${Math.min(matches.length, 12)} 条。`
      : "没有找到相关内容。试试“安装”“验证码”或“会话”，也可从手册目录选择任务。";
    matches.slice(0, 12).forEach((entry) => {
      const item = document.createElement("li");
      const link = document.createElement("a");
      link.href = entry.url;
      link.textContent = entry.title;
      const snippet = document.createElement("p");
      const plain = entry.text.replace(/[#*`|]/g, " ").replace(/\s+/g, " ");
      const start = Math.max(0, plain.toLocaleLowerCase().indexOf(terms[0]) - 30);
      snippet.textContent = `${start ? "…" : ""}${plain.slice(start, start + 120)}…`;
      item.append(link, snippet);
      list.append(item);
    });
  }
  input.addEventListener("input", update);
  clear.addEventListener("click", () => {
    input.value = "";
    update();
    input.focus();
  });
  input.addEventListener("keydown", (event) => {
    if (event.key === "Escape") clear.click();
  });
})();
