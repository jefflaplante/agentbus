// The hero animation: a broker with agents around it, and the traffic
// agentbus carries drawn as it would flow. Every message goes through the
// broker (the violet core). Requests are cyan, replies green, events amber;
// a refusal flashes coral at the broker and goes back to the sender as an
// error. Each message is also written to the live wire under the hero.
(() => {
  "use strict";
  const canvas = document.getElementById("bus");
  if (!canvas) return;
  const ctx = canvas.getContext("2d");
  const wire = document.getElementById("wire");
  const reduced = matchMedia("(prefers-reduced-motion: reduce)").matches;

  const C = {
    request: "#5ce1e6", response: "#3ddc97", event: "#ffb547", error: "#ff6b81",
    hub: "#9b8cff", text: "#e8ecf4", faint: "rgba(141,150,170,.55)",
  };

  const AGENTS = [
    { id: "claude-code", role: "MCP" },
    { id: "weather", role: "weather.forecast" },
    { id: "researcher", role: "research" },
    { id: "coordinator", role: "" },
    { id: "worker-1", role: "textstats" },
    { id: "worker-2", role: "textstats" },
    { id: "conduit-home", role: "" },
    { id: "ops", role: "send_to: [worker-1]" },
    { id: "grafana", role: "observer" },
  ];

  let W = 0, H = 0, dpr = 1, hub = { x: 0, y: 0, r: 34 }, t0 = performance.now();
  const mouse = { x: -1e4, y: -1e4, px: 0, py: 0 };
  const packets = [], ripples = [], stamps = [];
  let hubFlash = 0, hubErr = 0, visible = true, last = performance.now();

  const nodes = AGENTS.map((a, i) => ({
    ...a, i, angle: (i / AGENTS.length) * Math.PI * 2 + 0.3, x: 0, y: 0, pulse: 0, ring: i % 2,
    wobble: Math.random() * Math.PI * 2,
  }));
  const byId = Object.fromEntries(nodes.map((n) => [n.id, n]));

  function resize() {
    const r = canvas.getBoundingClientRect();
    dpr = Math.min(window.devicePixelRatio || 1, 2);
    W = r.width; H = r.height;
    canvas.width = Math.round(W * dpr); canvas.height = Math.round(H * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    const narrow = W < 980;
    // On a narrow screen the copy fills the top; the bus gets the space
    // left under it (the hero reserves it with padding).
    hub.x = narrow ? W * 0.5 : W * 0.73;
    hub.y = narrow ? H - Math.min(260, H * 0.27) : H * 0.44;
    hub.r = narrow ? 26 : 34;
  }

  // Agents sit on two ellipses around the broker and drift slowly.
  function layout(time) {
    const narrow = W < 980;
    const rx = narrow ? Math.min(W * 0.4, 300) : Math.min(W * 0.23, 360);
    const ry = narrow ? Math.min(150, H * 0.16) : Math.min(H * 0.3, 270);
    for (const n of nodes) {
      const k = n.ring ? 1 : 0.68;
      const a = n.angle + time * 0.000035 * (n.ring ? 1 : -1.4);
      const wob = Math.sin(time * 0.0009 + n.wobble) * 6;
      let x = hub.x + Math.cos(a) * rx * k + wob;
      let y = hub.y + Math.sin(a) * ry * k + Math.cos(time * 0.0007 + n.wobble) * 5;
      // lean away from the cursor a little
      const dx = x - mouse.x, dy = y - mouse.y, d = Math.hypot(dx, dy);
      if (d < 160) { x += (dx / d) * (160 - d) * 0.18; y += (dy / d) * (160 - d) * 0.18; }
      n.x += (x - n.x) * 0.12; n.y += (y - n.y) * 0.12;
    }
  }

  // A packet travels from a node to the broker, then from the broker to
  // each of its targets. onArrive runs at each target.
  function send(type, from, to, opts = {}) {
    const p = {
      type, from, to: Array.isArray(to) ? to : [to], t: 0, leg: 0,
      speed: (opts.speed || 1) * (0.0011 + Math.random() * 0.0003), trail: [],
      refuse: opts.refuse, onArrive: opts.onArrive, bend: (Math.random() - 0.5) * 0.9,
    };
    packets.push(p);
    log(opts.log || { type, from: from.id, to: p.to.map((n) => n.id).join(", "), topic: opts.topic });
    return p;
  }

  function curve(a, b, bend, t) {
    const mx = (a.x + b.x) / 2, my = (a.y + b.y) / 2;
    const nx = -(b.y - a.y), ny = b.x - a.x;
    const cx = mx + nx * bend * 0.35, cy = my + ny * bend * 0.35;
    const u = 1 - t;
    return { x: u * u * a.x + 2 * u * t * cx + t * t * b.x, y: u * u * a.y + 2 * u * t * cy + t * t * b.y };
  }

  const rand = (a) => a[Math.floor(Math.random() * a.length)];
  const inbox = (id) => `a2a/v1/agents/${id}/inbox`;

  const scenarios = [
    // a task to a capability, served by the one weather agent
    [5, () => request("claude-code", "weather", { topic: "a2a/v1/tasks/weather.forecast", task: true })],
    // a task to a shared queue: one of two workers gets it
    [5, () => request("coordinator", rand(["worker-1", "worker-2"]), { topic: "a2a/v1/tasks/textstats", task: true })],
    [4, () => request("conduit-home", "researcher", { topic: inbox("researcher") })],
    [3, () => request("researcher", "claude-code", { topic: inbox("claude-code") })],
    [3, () => broadcast(rand(["weather", "worker-1", "worker-2"]), rand(["jobs/done", "forecast/updated", "jobs/started"]))],
    [2, () => refuse("ops", "researcher", "not_authorized")],
    [1, () => refuse(rand(["coordinator", "conduit-home"]), "weather", "expired")],
    [1, () => refuse("worker-2", "grafana", "no_route")],
  ];
  const total = scenarios.reduce((s, [w]) => s + w, 0);
  function scenario() {
    let r = Math.random() * total;
    for (const [w, fn] of scenarios) { if ((r -= w) < 0) return fn(); }
  }

  function request(fromId, toId, { topic, task }) {
    const from = byId[fromId], to = byId[toId];
    send("request", from, to, {
      topic,
      log: { type: "request", from: fromId, to: task ? `${toId} (task)` : toId, topic },
      onArrive: () => setTimeout(() => send("response", to, from, { topic: inbox(fromId) }), 260 + Math.random() * 500),
    });
  }

  function broadcast(fromId, name) {
    const from = byId[fromId];
    const subs = nodes.filter((n) => n !== from && (n.id === "grafana" || Math.random() < 0.55));
    send("event", from, subs, { topic: `a2a/v1/events/${fromId}/${name}`, log: { type: "event", from: fromId, to: "*", topic: `a2a/v1/events/${fromId}/${name}` } });
  }

  function refuse(fromId, toId, code) {
    const from = byId[fromId];
    send("request", from, byId[toId], { topic: inbox(toId), refuse: { code, back: from } });
  }

  function log(item) {
    if (!wire) return;
    const d = new Date();
    const ts = d.toTimeString().slice(0, 8) + "." + String(d.getMilliseconds()).padStart(3, "0");
    const el = document.createElement("span");
    el.className = "wire-item " + item.type;
    el.innerHTML = `<span class="t">${ts}</span><span class="ty">${item.type}</span><span>${esc(item.from)} → ${esc(item.to)}</span><span class="topic">${esc(item.topic || "")}</span>`;
    wire.prepend(el);
    while (wire.children.length > 14) wire.lastChild.remove();
  }
  const esc = (s) => String(s).replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" })[c]);

  function step(dt) {
    for (let i = packets.length - 1; i >= 0; i--) {
      const p = packets[i];
      p.t += p.speed * dt;
      if (p.t >= 1) {
        p.t = 0;
        if (p.leg === 0) {
          hubFlash = 1;
          if (p.refuse) {
            hubErr = 1;
            stamps.push({ code: p.refuse.code, life: 1, x: hub.x, y: hub.y - hub.r - 18 });
            packets.splice(i, 1);
            send("error", { id: "$broker", x: hub.x, y: hub.y }, p.refuse.back, {
              log: { type: "error", from: "$broker", to: p.refuse.back.id, topic: `${p.refuse.code} · ${inbox(p.refuse.back.id)}` },
            });
            continue;
          }
          p.leg = 1; p.trail.length = 0;
        } else {
          for (const n of p.to) { n.pulse = 1; ripples.push({ x: n.x, y: n.y, r: 6, life: 1, color: C[p.type] }); }
          if (p.onArrive) p.onArrive();
          packets.splice(i, 1);
          continue;
        }
      }
    }
    for (const n of nodes) n.pulse = Math.max(0, n.pulse - dt * 0.002);
    hubFlash = Math.max(0, hubFlash - dt * 0.003);
    hubErr = Math.max(0, hubErr - dt * 0.0016);
    for (let i = ripples.length - 1; i >= 0; i--) { const r = ripples[i]; r.r += dt * 0.07; r.life -= dt * 0.0013; if (r.life <= 0) ripples.splice(i, 1); }
    for (let i = stamps.length - 1; i >= 0; i--) { const s = stamps[i]; s.life -= dt * 0.0006; s.y -= dt * 0.012; if (s.life <= 0) stamps.splice(i, 1); }
  }

  function packetPos(p, from, to) {
    // leg 0: sender to broker; leg 1: broker to target
    return curve(from, to, p.bend, easeInOut(p.t));
  }
  const easeInOut = (t) => (t < 0.5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2);

  function draw(time) {
    ctx.clearRect(0, 0, W, H);
    const par = { x: (mouse.px - 0.5) * -14, y: (mouse.py - 0.5) * -10 };
    ctx.save();
    ctx.translate(par.x, par.y);

    // dot field
    ctx.fillStyle = "rgba(120,130,160,.10)";
    const g = 34, ox = (time * 0.004) % g;
    for (let x = -g + ox; x < W + g; x += g) for (let y = 0; y < H; y += g) ctx.fillRect(x, y, 1.2, 1.2);

    // spokes from every agent to the broker
    for (const n of nodes) {
      const grad = ctx.createLinearGradient(n.x, n.y, hub.x, hub.y);
      grad.addColorStop(0, `rgba(155,140,255,${0.05 + n.pulse * 0.4})`);
      grad.addColorStop(1, "rgba(155,140,255,.14)");
      ctx.strokeStyle = grad; ctx.lineWidth = 1;
      ctx.setLineDash([2, 7]); ctx.lineDashOffset = -time * 0.02;
      ctx.beginPath(); ctx.moveTo(n.x, n.y); ctx.lineTo(hub.x, hub.y); ctx.stroke();
    }
    ctx.setLineDash([]);

    // broker
    const pulse = 1 + Math.sin(time * 0.002) * 0.04 + hubFlash * 0.16;
    const core = hubErr > 0.01 ? mix(C.hub, C.error, hubErr) : C.hub;
    const glow = ctx.createRadialGradient(hub.x, hub.y, 0, hub.x, hub.y, hub.r * 4.2 * pulse);
    glow.addColorStop(0, hexA(core, 0.55)); glow.addColorStop(0.35, hexA(core, 0.14)); glow.addColorStop(1, hexA(core, 0));
    ctx.fillStyle = glow; ctx.beginPath(); ctx.arc(hub.x, hub.y, hub.r * 4.2 * pulse, 0, 7); ctx.fill();
    ctx.lineWidth = 1.4;
    for (let k = 0; k < 3; k++) {
      ctx.strokeStyle = hexA(core, 0.5 - k * 0.13);
      ctx.setLineDash([10 + k * 6, 8 + k * 4]);
      ctx.lineDashOffset = time * (k % 2 ? -0.03 : 0.022) * (1 + hubFlash * 3);
      ctx.beginPath(); ctx.arc(hub.x, hub.y, hub.r * (1.35 + k * 0.42) * pulse, 0, 7); ctx.stroke();
    }
    ctx.setLineDash([]);
    const coreG = ctx.createRadialGradient(hub.x - hub.r * 0.3, hub.y - hub.r * 0.3, 2, hub.x, hub.y, hub.r * pulse);
    coreG.addColorStop(0, "#efeaff"); coreG.addColorStop(0.45, core); coreG.addColorStop(1, "#2b2380");
    ctx.fillStyle = coreG; ctx.beginPath(); ctx.arc(hub.x, hub.y, hub.r * pulse, 0, 7); ctx.fill();
    ctx.fillStyle = "rgba(10,11,20,.85)"; ctx.font = `700 ${W < 980 ? 9 : 11}px "JetBrains Mono", monospace`;
    ctx.textAlign = "center"; ctx.textBaseline = "middle";
    ctx.fillText("broker", hub.x, hub.y);

    // ripples
    for (const r of ripples) {
      ctx.strokeStyle = hexA(r.color, r.life * 0.6); ctx.lineWidth = 1.5;
      ctx.beginPath(); ctx.arc(r.x, r.y, r.r, 0, 7); ctx.stroke();
    }

    // packets, with trails, drawn additively
    ctx.globalCompositeOperation = "lighter";
    for (const p of packets) {
      const color = C[p.type];
      const legs = p.leg === 0 ? [[p.from, hub]] : p.to.map((n) => [hub, n]);
      if (!p.trails) p.trails = [];
      legs.forEach(([a, b], k) => {
        const pos = packetPos(p, a, b);
        const tr = (p.trails[k + p.leg * 100] ||= []);
        tr.push(pos); if (tr.length > 16) tr.shift();
        for (let j = 1; j < tr.length; j++) {
          ctx.strokeStyle = hexA(color, (j / tr.length) * 0.55);
          ctx.lineWidth = (j / tr.length) * 3.2;
          ctx.beginPath(); ctx.moveTo(tr[j - 1].x, tr[j - 1].y); ctx.lineTo(tr[j].x, tr[j].y); ctx.stroke();
        }
        const g2 = ctx.createRadialGradient(pos.x, pos.y, 0, pos.x, pos.y, 14);
        g2.addColorStop(0, hexA(color, 0.9)); g2.addColorStop(1, hexA(color, 0));
        ctx.fillStyle = g2; ctx.beginPath(); ctx.arc(pos.x, pos.y, 14, 0, 7); ctx.fill();
        ctx.fillStyle = "#fff"; ctx.beginPath(); ctx.arc(pos.x, pos.y, 2.2, 0, 7); ctx.fill();
      });
    }
    ctx.globalCompositeOperation = "source-over";

    // agents
    const small = W < 600;
    for (const n of nodes) {
      const near = Math.hypot(n.x - mouse.x, n.y - mouse.y) < 70;
      const r = (small ? 5 : 7) + n.pulse * 4 + (near ? 2 : 0);
      ctx.fillStyle = "#0b0f18"; ctx.strokeStyle = near ? C.request : `rgba(232,236,244,${0.55 + n.pulse * 0.45})`;
      ctx.lineWidth = 2;
      ctx.beginPath(); ctx.arc(n.x, n.y, r, 0, 7); ctx.fill(); ctx.stroke();
      ctx.fillStyle = near || n.pulse > 0.2 ? C.text : "rgba(200,207,222,.78)";
      ctx.font = `500 ${small ? 10.5 : 12}px "JetBrains Mono", monospace`;
      // labels point away from the broker, unless that runs off the canvas
      const wide = Math.max(ctx.measureText(n.id).width, n.role && !small ? n.role.length * 6.4 : 0) + 18;
      let left = n.x < hub.x - 4;
      if (!left && n.x + wide > W - par.x - 6) left = true;
      if (left && n.x - wide < -par.x + 6) left = false;
      ctx.textAlign = left ? "right" : "left";
      const lx = n.x + (left ? -14 : 14);
      ctx.fillText(n.id, lx, n.y - 1);
      if (n.role && !small) { ctx.fillStyle = C.faint; ctx.font = `400 10.5px "JetBrains Mono", monospace`; ctx.fillText(n.role, lx, n.y + 13); }
    }

    // refusal stamps
    for (const s of stamps) {
      ctx.save();
      ctx.globalAlpha = Math.min(1, s.life * 1.6);
      ctx.translate(s.x, s.y); ctx.rotate(-0.06);
      ctx.font = `700 12px "JetBrains Mono", monospace`;
      const w = ctx.measureText(s.code).width + 16;
      ctx.strokeStyle = C.error; ctx.lineWidth = 2; ctx.fillStyle = "rgba(20,8,14,.8)";
      roundRect(-w / 2, -12, w, 24, 5); ctx.fill(); ctx.stroke();
      ctx.fillStyle = C.error; ctx.textAlign = "center"; ctx.fillText(s.code, 0, 1);
      ctx.restore();
    }
    ctx.restore();
  }

  function roundRect(x, y, w, h, r) {
    ctx.beginPath(); ctx.moveTo(x + r, y); ctx.arcTo(x + w, y, x + w, y + h, r); ctx.arcTo(x + w, y + h, x, y + h, r);
    ctx.arcTo(x, y + h, x, y, r); ctx.arcTo(x, y, x + w, y, r); ctx.closePath();
  }
  function hexA(hex, a) {
    const n = parseInt(hex.slice(1), 16);
    return `rgba(${n >> 16},${(n >> 8) & 255},${n & 255},${Math.max(0, Math.min(1, a))})`;
  }
  function mix(a, b, k) {
    const x = parseInt(a.slice(1), 16), y = parseInt(b.slice(1), 16);
    const ch = (s) => Math.round(((x >> s) & 255) * (1 - k) + ((y >> s) & 255) * k);
    return "#" + ((1 << 24) | (ch(16) << 16) | (ch(8) << 8) | ch(0)).toString(16).slice(1);
  }

  let nextAt = 0;
  function frame(now) {
    const dt = Math.min(50, now - last); last = now;
    const time = now - t0;
    layout(time);
    if (now > nextAt && packets.length < 9) { scenario(); nextAt = now + 700 + Math.random() * 1100; }
    step(dt);
    draw(time);
    if (visible) requestAnimationFrame(frame);
  }

  canvas.addEventListener("pointermove", (e) => {
    const r = canvas.getBoundingClientRect();
    mouse.x = e.clientX - r.left; mouse.y = e.clientY - r.top;
    mouse.px = mouse.x / W; mouse.py = mouse.y / H;
  });
  canvas.addEventListener("pointerleave", () => { mouse.x = mouse.y = -1e4; mouse.px = mouse.py = 0.5; });
  // a click makes the nearest agent broadcast an event
  canvas.addEventListener("click", (e) => {
    const r = canvas.getBoundingClientRect();
    const x = e.clientX - r.left, y = e.clientY - r.top;
    let best = nodes[0], bd = Infinity;
    for (const n of nodes) { const d = Math.hypot(n.x - x, n.y - y); if (d < bd) { bd = d; best = n; } }
    ripples.push({ x: best.x, y: best.y, r: 6, life: 1, color: C.event });
    broadcast(best.id, "clicked");
  });

  resize();
  layout(0);
  for (const n of nodes) { n.x = hub.x; n.y = hub.y; }
  window.addEventListener("resize", resize);

  if (reduced) {
    for (let i = 0; i < 60; i++) layout(i * 16);
    draw(0);
    log({ type: "request", from: "claude-code", to: "weather (task)", topic: "a2a/v1/tasks/weather.forecast" });
    log({ type: "response", from: "weather", to: "claude-code", topic: inbox("claude-code") });
    return;
  }
  // Pause when the hero scrolls away or the tab is hidden.
  new IntersectionObserver(([e]) => {
    const was = visible; visible = e.isIntersecting && !document.hidden;
    if (visible && !was) { last = performance.now(); requestAnimationFrame(frame); }
  }).observe(canvas);
  document.addEventListener("visibilitychange", () => {
    const was = visible; visible = !document.hidden && canvas.getBoundingClientRect().bottom > 0;
    if (visible && !was) { last = performance.now(); requestAnimationFrame(frame); }
  });
  requestAnimationFrame(frame);
})();
