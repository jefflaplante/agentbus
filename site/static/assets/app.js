// The site: a hash router between the home page and the docs reader,
// search over docs.json, and the home page's small interactions.
(() => {
  "use strict";
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
  const reduced = matchMedia("(prefers-reduced-motion: reduce)").matches;

  let DOCS = null, BY = {};
  const docsReady = fetch("docs.json").then((r) => r.json()).then((d) => {
    DOCS = d.docs; DOCS.forEach((x, i) => { x.i = i; BY[x.slug] = x; });
    renderDocGroups();
    return d;
  });

  // ============================== ROUTER ==============================
  function route() {
    const h = decodeURIComponent(location.hash.replace(/^#\/?/, ""));
    const [kind, a, ...rest] = h.split("/");
    closeMenu();
    if (kind === "docs" && a) return showDoc(a, rest.join("/"));
    showHome(kind === "home" ? a : "");
  }

  function showHome(section) {
    const fromDocs = $("#home").hidden;
    document.body.classList.remove("in-docs");
    $("#docs").hidden = true; $("#home").hidden = false;
    document.title = "agentbus · a message bus for AI agents";
    $$(".nav-links a").forEach((a) => a.classList.toggle("active", a.getAttribute("href") === "#/home/" + section));
    const el = section && document.getElementById(section);
    if (el) requestAnimationFrame(() => el.scrollIntoView({ behavior: fromDocs || reduced ? "auto" : "smooth" }));
    else if (fromDocs || !section) window.scrollTo({ top: 0 });
    onScroll();
  }

  async function showDoc(slug, anchor) {
    document.body.classList.add("in-docs");
    $("#home").hidden = true; $("#docs").hidden = false;
    $$(".nav-links a").forEach((a) => a.classList.toggle("active", a.getAttribute("href") === "#/docs/overview"));
    if (!DOCS) { $("#doc-body").innerHTML = `<p class="doc-loading">loading…</p>`; await docsReady; }
    const doc = BY[slug];
    if (!doc) { location.replace("#/docs/overview"); return; }
    const current = $("#doc").dataset.slug;
    if (current !== slug) renderDoc(doc);
    if (anchor) {
      const target = document.getElementById(anchor);
      if (target) {
        requestAnimationFrame(() => {
          target.scrollIntoView({ behavior: current === slug && !reduced ? "smooth" : "auto" });
          target.classList.remove("flash"); void target.offsetWidth; target.classList.add("flash");
        });
      }
    } else if (current !== slug) window.scrollTo({ top: 0 });
  }

  // ============================== DOCS ==============================
  function renderDoc(doc) {
    document.title = `${doc.title} · agentbus`;
    $("#doc").dataset.slug = doc.slug;
    renderSidebar(doc.slug);
    $("#doc-meta").innerHTML =
      `<span class="crumb">${esc(doc.group)}</span>` +
      `<a class="src" href="https://github.com/jefflaplante/agentbus/blob/main/${esc(doc.source)}">${esc(doc.source)} ↗</a>`;
    const body = $("#doc-body");
    body.innerHTML = `<h1 class="doc-title">${esc(doc.title)}</h1>` + doc.html;
    body.style.animation = "none"; void body.offsetWidth; body.style.animation = "";

    for (const h of $$("h2[id], h3[id]", body)) {
      const a = document.createElement("a");
      a.className = "anchor"; a.href = `#/docs/${doc.slug}/${h.id}`; a.textContent = "#"; a.setAttribute("aria-label", "Link to this section");
      h.prepend(a);
    }
    for (const a of $$("a[href^='http']", body)) { a.target = "_blank"; a.rel = "noopener"; }
    for (const code of $$("pre > code", body)) {
      const lang = (code.className.match(/language-([\w-]+)/) || [])[1] || guessLang(code.textContent);
      code.innerHTML = window.highlight(code.textContent, lang);
      addCopy(code.parentElement, code.textContent);
    }
    const toc = $("#toc");
    toc.innerHTML = doc.headings.length
      ? `<h4>On this page</h4>` + doc.headings.map((h) => `<a class="l${h.level}" href="#/docs/${doc.slug}/${esc(h.id)}" data-id="${esc(h.id)}">${esc(h.text)}</a>`).join("")
      : "";
    const prev = DOCS[doc.i - 1], next = DOCS[doc.i + 1];
    $("#doc-pager").innerHTML =
      (prev ? `<a class="prev" href="#/docs/${prev.slug}"><small>← ${esc(prev.group)}</small><b>${esc(prev.title)}</b></a>` : "") +
      (next ? `<a class="next" href="#/docs/${next.slug}"><small>${esc(next.group)} →</small><b>${esc(next.title)}</b></a>` : "");
    spy();
  }

  function guessLang(text) {
    if (/^\s*[{[]/.test(text)) return "json";
    if (/^\s*(agentbus|\$ |export |make |go |sudo |cd )/m.test(text)) return "bash";
    return "";
  }

  function renderSidebar(active) {
    const groups = [];
    for (const d of DOCS) {
      let g = groups.find((x) => x.name === d.group);
      if (!g) groups.push((g = { name: d.group, docs: [] }));
      g.docs.push(d);
    }
    const sb = $("#sidebar");
    const collapsed = matchMedia("(max-width: 820px)").matches;
    sb.classList.toggle("collapsed", collapsed);
    sb.innerHTML =
      `<button class="sidebar-toggle" type="button" aria-expanded="${!collapsed}">${esc(BY[active].title)}<span>▾</span></button>` +
      groups.map((g) => `<h4>${esc(g.name)}</h4>` + g.docs.map((d) =>
        `<a href="#/docs/${d.slug}" class="${d.slug === active ? "active" : ""}">${esc(d.title)}</a>`).join("")).join("");
    $(".sidebar-toggle", sb).onclick = (e) => {
      sb.classList.toggle("collapsed");
      e.currentTarget.setAttribute("aria-expanded", !sb.classList.contains("collapsed"));
    };
  }

  // Highlight the table-of-contents entry for the heading in view.
  let spyObserver;
  function spy() {
    spyObserver?.disconnect();
    const links = new Map($$("#toc a").map((a) => [a.dataset.id, a]));
    if (!links.size) return;
    const visible = new Set();
    spyObserver = new IntersectionObserver((entries) => {
      for (const e of entries) e.isIntersecting ? visible.add(e.target.id) : visible.delete(e.target.id);
      const heads = $$("#doc-body h2[id], #doc-body h3[id]");
      let current = heads.find((h) => visible.has(h.id));
      if (!current) current = [...heads].reverse().find((h) => h.getBoundingClientRect().top < 120);
      links.forEach((a, id) => a.classList.toggle("active", current && id === current.id));
    }, { rootMargin: "-70px 0px -65% 0px" });
    $$("#doc-body h2[id], #doc-body h3[id]").forEach((h) => spyObserver.observe(h));
  }

  function renderDocGroups() {
    const groups = {};
    DOCS.forEach((d) => (groups[d.group] ||= []).push(d));
    $("#doc-groups").innerHTML = Object.entries(groups).map(([name, docs]) =>
      `<div class="doc-group reveal"><h3>${esc(name)}</h3>${docs.map((d) => `<a href="#/docs/${d.slug}">${esc(d.title)}</a>`).join("")}</div>`).join("");
    $$("#doc-groups .reveal").forEach((el) => revealer.observe(el));
  }

  // ============================== SEARCH ==============================
  const search = $("#search"), q = $("#search-q"), results = $("#search-results");
  let sel = 0;
  function openSearch() {
    search.hidden = false; q.value = ""; renderResults(""); q.focus();
    docsReady.then(() => renderResults(q.value));
  }
  function closeSearch() { search.hidden = true; }
  function renderResults(query) {
    const terms = query.toLowerCase().split(/\s+/).filter(Boolean);
    if (!DOCS) { results.innerHTML = `<li class="empty">loading…</li>`; return; }
    let list;
    if (!terms.length) {
      list = DOCS.slice(0, 8).map((d) => ({ d, snip: "" }));
    } else {
      list = [];
      for (const d of DOCS) {
        const title = d.title.toLowerCase(), text = d.text.toLowerCase();
        let score = 0, ok = true, head = null;
        for (const t of terms) {
          const inTitle = title.includes(t);
          const h = d.headings.find((x) => x.text.toLowerCase().includes(t));
          const count = text.split(t).length - 1;
          if (!inTitle && !h && !count) { ok = false; break; }
          score += (inTitle ? 40 : 0) + (h ? 12 : 0) + Math.min(count, 25);
          if (h && !head) head = h;
        }
        if (!ok) continue;
        const at = text.indexOf(terms[0]);
        let snip = at < 0 ? "" : d.text.slice(Math.max(0, at - 50), at + 110);
        snip = esc(snip);
        for (const t of terms) snip = snip.replace(new RegExp(t.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "gi"), (m) => `<mark>${m}</mark>`);
        list.push({ d, score, head, snip: (at > 50 ? "…" : "") + snip + "…" });
      }
      list.sort((a, b) => b.score - a.score);
      list = list.slice(0, 12);
    }
    sel = 0;
    results.innerHTML = list.length
      ? list.map((r, i) => `<li><a href="#/docs/${r.d.slug}${r.head ? "/" + esc(r.head.id) : ""}" class="${i === 0 ? "sel" : ""}">` +
          `<span class="rt">${esc(r.d.title)}${r.head ? ` <span class="rg">› ${esc(r.head.text)}</span>` : ""}</span><span class="rg">${esc(r.d.group)}</span>` +
          (r.snip ? `<span class="rs">${r.snip}</span>` : "") + `</a></li>`).join("")
      : `<li class="empty">Nothing matches “${esc(query)}”.</li>`;
  }
  q.addEventListener("input", () => renderResults(q.value));
  q.addEventListener("keydown", (e) => {
    const items = $$("a", results);
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      sel = (sel + (e.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
      items.forEach((a, i) => a.classList.toggle("sel", i === sel));
      items[sel]?.scrollIntoView({ block: "nearest" });
    } else if (e.key === "Enter" && items[sel]) {
      location.hash = items[sel].getAttribute("href"); closeSearch();
    }
  });
  results.addEventListener("click", (e) => { if (e.target.closest("a")) closeSearch(); });
  search.addEventListener("click", (e) => { if (e.target === search) closeSearch(); });
  $("#search-open").addEventListener("click", openSearch);
  document.addEventListener("keydown", (e) => {
    const typing = /INPUT|TEXTAREA/.test(document.activeElement?.tagName);
    if ((e.key === "/" && !typing) || (e.key === "k" && (e.metaKey || e.ctrlKey))) { e.preventDefault(); openSearch(); }
    else if (e.key === "Escape" && !search.hidden) closeSearch();
  });

  // ============================== HOME ==============================
  const nav = $("#nav");
  function onScroll() { nav.classList.toggle("solid", window.scrollY > 40); }
  window.addEventListener("scroll", onScroll, { passive: true });

  const menu = $("#menu"), links = $(".nav-links");
  menu.addEventListener("click", () => {
    const open = !links.classList.contains("open");
    links.classList.toggle("open", open); menu.setAttribute("aria-expanded", open);
  });
  function closeMenu() { links.classList.remove("open"); menu.setAttribute("aria-expanded", "false"); }

  const revealer = new IntersectionObserver((entries) => {
    for (const e of entries) if (e.isIntersecting) { e.target.classList.add("in"); revealer.unobserve(e.target); countUp(e.target); }
  }, { threshold: 0.12, rootMargin: "0px 0px -40px 0px" });
  $$(".reveal").forEach((el) => revealer.observe(el));

  function countUp(el) {
    for (const b of $$("[data-count]", el)) {
      const end = +b.dataset.count, start = performance.now(), dur = reduced ? 0 : 900;
      const tick = (now) => {
        const k = dur ? Math.min(1, (now - start) / dur) : 1;
        b.textContent = Math.round(end * (1 - Math.pow(1 - k, 3)));
        if (k < 1) requestAnimationFrame(tick);
      };
      requestAnimationFrame(tick);
    }
  }

  // a glow that follows the pointer around each pattern card's border
  for (const card of $$(".pattern")) {
    card.addEventListener("pointermove", (e) => {
      const r = card.getBoundingClientRect();
      card.style.setProperty("--mx", `${e.clientX - r.left}px`);
      card.style.setProperty("--my", `${e.clientY - r.top}px`);
    });
  }

  // console screenshots
  const captions = {
    "console-overview": "Broker status, throughput over time, and request outcomes by target.",
    "console-agents": "Every agent: what the broker knows and what its presence card says.",
    "console-traffic": "Every message the connection can see, filtered by agent, type or text.",
    "console-compose": "Send a request, task, send or emit, and keep each reply with its message.",
    "console-inbox": "Requests sent to the console wait for you to answer or refuse them.",
    "config-client": "Add an agent and the console shows where its token is and how to start it.",
  };
  const shot = $("#shot"), screen = $("#screen");
  for (const tab of $$("#console .tabs button")) {
    tab.addEventListener("click", () => {
      $$("#console .tabs button").forEach((b) => b.setAttribute("aria-selected", b === tab));
      const name = tab.dataset.shot;
      shot.classList.add("swap");
      setTimeout(() => {
        shot.src = `r/docs/images/${name}.svg`; shot.alt = `agentbus console, ${tab.textContent.slice(2)} tab`;
        $("#shot-caption").textContent = captions[name];
        shot.onload = () => shot.classList.remove("swap");
      }, 180);
    });
  }
  // tilt the screenshot toward the pointer
  if (!reduced) {
    screen.addEventListener("pointermove", (e) => {
      const r = screen.getBoundingClientRect();
      const x = (e.clientX - r.left) / r.width - 0.5, y = (e.clientY - r.top) / r.height - 0.5;
      screen.style.transform = `rotateY(${x * 6}deg) rotateX(${-y * 5}deg)`;
    });
    screen.addEventListener("pointerleave", () => (screen.style.transform = ""));
  }

  // language tabs
  for (const tab of $$("#languages .tabs button")) {
    tab.addEventListener("click", () => {
      $$("#languages .tabs button").forEach((b) => b.setAttribute("aria-selected", b === tab));
      $$("#languages .pane").forEach((p) => p.classList.toggle("active", p.dataset.pane === tab.dataset.pane));
    });
  }
  for (const code of $$("#home pre.code code")) {
    const text = code.textContent;
    code.innerHTML = window.highlight(text, code.dataset.lang);
    addCopy(code.parentElement, text);
  }

  function addCopy(pre, text) {
    const b = document.createElement("button");
    b.className = "copy"; b.type = "button"; b.textContent = "copy";
    b.addEventListener("click", () => copy(b, text));
    pre.appendChild(b);
  }
  function copy(btn, text) {
    navigator.clipboard?.writeText(text).then(() => {
      btn.textContent = "copied"; btn.classList.add("done");
      setTimeout(() => { btn.textContent = "copy"; btn.classList.remove("done"); }, 1400);
    });
  }
  for (const b of $$("[data-copy-text]")) b.addEventListener("click", () => copy(b, b.dataset.copyText));

  // ============================== TYPED TERMINAL ==============================
  const SESSION = [
    { cmd: "agentbus serve &", out: [
      ["ok", "● No config found, so agentbus created one and started with it:"],
      ["o", "  config  ~/.config/agentbus/broker.json"],
      ["o", "  listen  127.0.0.1:1883   agents  admin"],
    ] },
    { cmd: "agentbus config agent add weather --capability weather.forecast", out: [
      ["ok", "✓ added weather; a running broker accepts it within a second"],
      ["dim", "  Token file  ~/.config/agentbus/tokens/weather.token"],
    ] },
    { cmd: "agentbus --agent weather handle -c weather.forecast --exec ./forecast.sh &", out: [] },
    { cmd: `agentbus task -c weather.forecast --body '{"location":"Seattle, WA"}'`, out: [
      ["o", "{"],
      ["o", '  <span class="k">"location"</span>: <span class="s">"Seattle, Washington, US"</span>,'],
      ["o", '  <span class="k">"source"</span>: <span class="s">"National Weather Service"</span>,'],
      ["o", '  <span class="k">"periods"</span>: [{ <span class="k">"name"</span>: <span class="s">"Tonight"</span>, <span class="k">"temperature"</span>: <span class="acc">43</span>, …'],
      ["o", "}"],
    ] },
    { cmd: `agentbus --agent ops request --to researcher --body '{}'`, out: [
      ["err", 'agentbus: $broker: <span style="color:#ff6b81">not_authorized</span>: not allowed to send to researcher'],
    ] },
  ];
  const typed = $("#typed");
  async function play() {
    const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
    if (reduced) {
      typed.innerHTML = SESSION.map((s) => `<span class="p">$</span> <span class="c">${esc(s.cmd)}</span>\n` + s.out.map(([c, l]) => `<span class="${c}">${l}</span>`).join("\n")).join("\n");
      return;
    }
    for (;;) {
      typed.innerHTML = "";
      for (const s of SESSION) {
        const line = document.createElement("span");
        line.innerHTML = `<span class="p">$</span> <span class="c"></span><span class="cursor"></span>`;
        typed.appendChild(line);
        const c = $(".c", line);
        for (const ch of s.cmd) { c.textContent += ch; await sleep(18 + Math.random() * 40); }
        await sleep(350);
        $(".cursor", line).remove();
        typed.append("\n");
        for (const [cls, l] of s.out) {
          const o = document.createElement("span"); o.className = cls; o.innerHTML = l + "\n";
          typed.appendChild(o); typed.scrollTop = typed.scrollHeight;
          await sleep(90);
        }
        await sleep(900);
        typed.scrollTop = typed.scrollHeight;
      }
      const end = document.createElement("span");
      end.innerHTML = `<span class="p">$</span> <span class="cursor"></span>`;
      typed.appendChild(end); typed.scrollTop = typed.scrollHeight;
      await sleep(5200);
    }
  }
  play();

  window.addEventListener("hashchange", route);
  route();
})();
