// A small highlighter for the languages the docs use. Each language is a
// list of [class, regex] rules tried in order at every position; text no
// rule matches is escaped and kept as is.
window.highlight = (() => {
  "use strict";
  const esc = (s) => s.replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" })[c]);
  const words = (list) => new RegExp("\\b(?:" + list.split(" ").join("|") + ")\\b", "y");

  const L = {};
  L.go = [
    ["c", /\/\/[^\n]*|\/\*[\s\S]*?\*\//y],
    ["s", /"(?:\\.|[^"\\\n])*"|`[^`]*`|'(?:\\.|[^'\\])'/y],
    ["k", words("break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var")],
    ["b", words("nil true false iota any error string int int64 bool byte float64 len cap make new append")],
    ["n", /\b\d[\d_.xXa-fA-F]*\b/y],
    ["f", /\b[A-Za-z_]\w*(?=\()/y],
  ];
  L.python = [
    ["c", /#[^\n]*/y],
    ["s", /[rbf]?("""[\s\S]*?"""|'''[\s\S]*?'''|"(?:\\.|[^"\\\n])*"|'(?:\\.|[^'\\\n])*')/y],
    ["k", words("and as assert async await break class continue def del elif else except finally for from global if import in is lambda not or pass raise return try while with yield")],
    ["b", words("None True False self")],
    ["n", /\b\d[\d_.]*\b/y],
    ["f", /\b[A-Za-z_]\w*(?=\()/y],
  ];
  L.json = [
    ["key", /"(?:\\.|[^"\\])*"(?=\s*:)/y],
    ["s", /"(?:\\.|[^"\\])*"/y],
    ["b", words("true false null")],
    ["n", /-?\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b/y],
  ];
  L.yaml = [
    ["c", /#[^\n]*/y],
    ["key", /[A-Za-z_][\w.-]*(?=:(?:\s|$))/y],
    ["s", /"(?:\\.|[^"\\\n])*"|'[^'\n]*'/y],
    ["b", words("true false null yes no")],
    ["n", /\b\d+(?:\.\d+)?(?:ms|s|m|h)?\b/y],
  ];
  L.bash = [
    ["c", /(?<=^|\s)#[^\n]*/my],
    ["p", /^\$ /my],
    ["s", /"(?:\\.|[^"\\])*"|'[^']*'/y],
    ["f", /(?<![\w-])--?[A-Za-z][\w-]*/y],
    ["k", /\b(?:agentbus|sudo|export|cd|make|go|python3|source|unset|systemctl|journalctl|install|cp|scp|ssh|eval|curl|jq|mosquitto_sub|mosquitto_pub|claude)\b/y],
    ["n", /\$\{?[A-Za-z_]\w*\}?/y],
  ];
  L.toml = L.yaml;
  const alias = { sh: "bash", shell: "bash", console: "bash", zsh: "bash", golang: "go", py: "python", yml: "yaml", jsonc: "json" };

  return function highlight(code, lang) {
    const rules = L[alias[lang] || lang];
    if (!rules) return esc(code);
    let out = "", i = 0, plain = "";
    outer: while (i < code.length) {
      for (const [cls, re] of rules) {
        re.lastIndex = i;
        const m = re.exec(code);
        if (m && m[0].length) {
          out += esc(plain); plain = "";
          out += `<span class="hl-${cls}">${esc(m[0])}</span>`;
          i += m[0].length;
          continue outer;
        }
      }
      // skip to the end of the current word so keywords inside words don't match
      const w = /[A-Za-z0-9_]+|[\s\S]/y; w.lastIndex = i;
      const m = w.exec(code);
      plain += m[0]; i += m[0].length;
    }
    return out + esc(plain);
  };
})();
