"use strict";
// All peer-supplied text is rendered with textContent only; never markup.
const $ = (id) => document.getElementById(id);
const S = { st: null, room: null, msgs: [], older: [], more: true, reply: null, sums: {}, sigs: {}, toSig: "", gen: 0, shown: null, toRoom: null };
let token = "";
try { token = sessionStorage.getItem("a2p2.token") || ""; } catch (e) { /* storage blocked */ }
if (location.hash.length > 1) {
  token = decodeURIComponent(location.hash.slice(1));
  try { sessionStorage.setItem("a2p2.token", token); } catch (e) { /* ignore */ }
}
history.replaceState(null, "", location.pathname + location.search);

function el(tag, cls, ...kids) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  for (const k of kids) if (k != null) e.append(k.nodeType ? k : document.createTextNode(String(k)));
  return e;
}
function btn(label, cls, fn) {
  const b = el("button", cls, label);
  b.type = "button";
  b.addEventListener("click", (ev) => { ev.stopPropagation(); fn(ev); });
  return b;
}
function fp(id) { return String(id || "").slice(0, 8); }
function who(a) { return String(a || ""); }
function fmt(t) {
  const d = new Date(t), now = new Date();
  const hm = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  return d.toDateString() === now.toDateString() ? hm : d.toLocaleDateString([], { month: "short", day: "numeric" }) + " " + hm;
}
function ago(t) {
  const s = Math.max(0, (Date.now() - new Date(t).getTime()) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  return Math.floor(s / 86400) + "d ago";
}

async function api(path, body) {
  const opt = { headers: { Authorization: "Bearer " + token } };
  if (body !== undefined) {
    opt.method = "POST";
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  const r = await fetch(path, opt);
  let j = null;
  try { j = await r.json(); } catch (e) { /* non-JSON */ }
  if (r.status === 401) { showAuthFail(); throw new Error("unauthorized"); }
  if (!r.ok) throw new Error((j && j.error) || "request failed (" + r.status + ")");
  return j;
}
function showAuthFail() { $("app").hidden = true; $("authfail").hidden = false; }

const room = () => (S.st && S.st.rooms.find((r) => r.id === S.room)) || null;
const unreadFor = (id) => S.st.unread.filter((m) => m.room === id);
const isQuestion = (m) => m.kind === "question" && (!m.to || m.to.length === 0 || m.to.includes(S.st.human));

// ---- sidebar ----
function renderSide() {
  const ul = $("rooms");
  ul.replaceChildren();
  for (const r of S.st.rooms) {
    const n = unreadFor(r.id).length;
    const li = el("li", r.id === S.room ? "sel" : "");
    li.append(el("span", "l", r.label, el("div", "sub", r.members.length + (r.members.length === 1 ? " node" : " nodes") + " · " + (r.owner === S.st.node ? "owner" : "member"))));
    if (n) li.append(el("span", "badge", n));
    li.addEventListener("click", () => openRoom(r.id));
    ul.append(li);
  }
  const box = $("node");
  box.replaceChildren();
  const mine = new Set(S.st.listen || []);
  const queued = S.st.outbox.reduce((a, o) => a + o.queued, 0);
  box.append(el("div", "", el("strong", "", S.st.name), " ", el("span", "muted", fp(S.st.node))),
    el("div", "", el("span", "dot " + (mine.size ? "ok" : "")), "Network " + (mine.size ? "on" : "off (this machine only)")));
  for (const a of mine) box.append(el("div", "sub", a));
  box.append(el("h2", "", "Peers"));
  if (!S.st.peers.length) box.append(el("div", "sub", "No peers yet."));
  for (const p of S.st.peers) {
    const label = { ok: "reachable", problem: "unreachable", unknown: "not contacted yet" }[p.state] || "";
    box.append(el("div", "", el("span", "dot " + (p.state === "ok" ? "ok" : p.state === "problem" ? "bad" : "")), p.name || fp(p.node), " ",
      el("span", "muted", label)));
    if (p.last_error) box.append(el("div", "sub", p.last_error));
  }
  box.append(el("h2", "", "Outbox"), el("div", "sub", queued ? queued + " queued" : "empty"));
  box.append(el("h2", "", "Local agents"));
  for (const a of S.st.agents) box.append(el("div", "", a.label, " ", el("span", "sub", a.harness + " · " + ago(a.last_seen))));
}

// ---- needs you ----
function renderNeeds() {
  const qs = S.st.unread.filter(isQuestion);
  const sig = qs.map((m) => m.id).join();
  $("needs").hidden = !qs.length;
  if (sig === S.sigs.q) return;
  S.sigs.q = sig;
  const box = $("questions");
  box.replaceChildren();
  for (const m of qs) {
    const input = el("input");
    input.placeholder = "Type an answer";
    const answer = (t) => t.trim() && sendAnswer(m, t.trim());
    input.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); answer(input.value); } });
    const row = el("div", "row");
    for (const c of (m.body.choices || [])) row.append(btn(c, "pri", () => answer(c)));
    row.append(input, btn("Answer", "", () => answer(input.value)));
    box.append(el("div", "qcard", el("div", "sub", who(m.from) + " asks"), el("div", "txt", m.body.text || ""), row));
  }
}
async function sendAnswer(m, text) {
  try {
    await api("/api/send", { room: m.room, text, kind: "answer", reply_to: m.id });
    await api("/api/seen", { ids: [m.id] });
    refresh();
  } catch (e) { $("err").textContent = e.message; }
}

// ---- stream ----
function all() { return S.older.concat(S.msgs); }
function sumFor(m) {
  const s = S.sums[m.id];
  return s ? s.text : "";
}
function renderStream(force) {
  const list = all().filter((m) => m.kind !== "receipt");
  const sig = S.room + "|" + list.map((m) => m.id + sumFor(m)).join() + "|" + S.more;
  if (sig === S.sigs.s && !force) return;
  S.sigs.s = sig;
  const sc = $("stream");
  const atBottom = sc.scrollHeight - sc.scrollTop - sc.clientHeight < 40;
  const box = $("msgs");
  box.replaceChildren();
  for (const m of list) {
    const meta = el("div", "meta", el("span", "from", who(m.from)), el("span", "", fmt(m.created_at)));
    if (m.to && m.to.length) meta.append(el("span", "", "to " + m.to.map(who).join(", ")));
    if (m.kind === "question" || m.kind === "answer") meta.append(el("span", "kind", m.kind));
    if (m.from_node === S.st.node && sumFor(m)) meta.append(el("span", "", sumFor(m)));
    meta.append(btn("reply", "link", () => setReply(m)));
    const d = el("div", "msg" + (m.from === S.st.human ? " mine" : ""), meta, el("div", "body", m.body.text || ""));
    d.addEventListener("click", () => openTrace(m.id));
    box.append(d);
  }
  $("older").hidden = !S.more || !S.msgs.length;
  const quiet = !S.st.agents.some((a) => a.id !== S.st.human) && !list.length;
  $("empty").hidden = !quiet;
  if (atBottom) sc.scrollTop = sc.scrollHeight;
}
// The compose box is live only while the displayed history belongs to the
// selected room, and it names that room.
function updateCompose() {
  const r = room(), ready = !!r && S.shown === S.room;
  $("text").placeholder = r ? "Message #" + r.label + ". Enter sends, Shift+Enter adds a line." : "Select a room";
  const b = $("compose").querySelector("button[type=submit]");
  if (b) { b.disabled = !ready; b.textContent = r ? "Send to #" + r.label : "Send"; }
}
function renderHead() {
  const r = room();
  $("roomhead").textContent = r ? r.label : "";
  updateCompose();
  if (S.toRoom !== S.room) { S.toRoom = S.room; $("to").value = ""; }
  const senders = new Set();
  for (const m of all()) if (m.from !== S.st.human) senders.add(m.from);
  const sig = S.room + [...senders].join();
  if (sig === S.toSig) return;
  S.toSig = sig;
  const sel = $("to"), cur = sel.value;
  sel.replaceChildren(el("option", "", "Everyone"));
  sel.firstChild.value = "";
  for (const s of senders) { const o = el("option", "", s); o.value = s; sel.append(o); }
  sel.value = cur;
}
function setReply(m) {
  S.reply = m;
  const r = $("replying");
  r.hidden = false;
  r.replaceChildren("Replying to " + who(m.from) + ": " + (m.body.text || "").slice(0, 60) + " ", btn("cancel", "link", () => { S.reply = null; r.hidden = true; }));
  $("text").focus();
}

async function updateSummaries() {
  const mine = S.msgs.filter((m) => m.from_node === S.st.node && m.kind !== "receipt" && !(S.sums[m.id] && S.sums[m.id].done)).slice(-20);
  for (const m of mine) {
    try {
      const t = await api("/api/trace?id=" + encodeURIComponent(m.id));
      const c = {};
      for (const d of t.deliveries || []) c[d.state] = (c[d.state] || 0) + 1;
      const parts = Object.keys(c).map((k) => c[k] + " " + k);
      const done = !c.queued;
      S.sums[m.id] = { done, text: parts.length ? "[" + parts.join(", ") + "]" : "[local]" };
    } catch (e) { return; }
  }
}

async function openTrace(id) {
  try {
    const t = await api("/api/trace?id=" + encodeURIComponent(id));
    const b = $("tracebody");
    b.replaceChildren(el("h2", "", "Message " + id), el("div", "body", t.message.body.text || ""));
    const dt = el("table"), mt = el("table");
    dt.append(el("tr", "", ...["Node", "State", "Attempts", "Last error"].map((h) => el("th", "", h))));
    for (const d of t.deliveries || []) dt.append(el("tr", "", el("td", "", fp(d.node)), el("td", "", d.state), el("td", "", d.attempts), el("td", "", d.last_error || "")));
    mt.append(el("tr", "", ...["Agent", "Mark", "When"].map((h) => el("th", "", h))));
    for (const k of t.marks || []) mt.append(el("tr", "", el("td", "", k.agent), el("td", "", k.state), el("td", "", fmt(k.at))));
    b.append(el("h2", "", "Delivery"), (t.deliveries || []).length ? dt : el("div", "sub", "Local only; no other nodes."),
      el("h2", "", "Seen / handled"), (t.marks || []).length ? mt : el("div", "sub", "Nobody has seen it yet."));
    $("trace").showModal();
  } catch (e) { $("err").textContent = e.message; }
}

// ---- data flow ----
async function loadHistory(rid, extra) {
  const q = "/api/history?room=" + encodeURIComponent(rid) + "&limit=100" + (extra || "");
  return api(q);
}
// switchTo makes id the current room and discards everything shown for the
// previous one; responses still in flight for it are dropped by generation.
function switchTo(id) {
  S.gen++; S.room = id; S.shown = null; S.msgs = []; S.older = []; S.more = true; S.reply = null; S.toSig = "";
  S.sigs.s = ""; S.sums = {};
  $("msgs").replaceChildren();
  $("replying").hidden = true;
  $("older").hidden = true;
  if (S.st) { renderHead(); renderSide(); }
}
async function openRoom(id) {
  if (id === S.room && S.shown === id) return;
  switchTo(id);
  await refresh(true);
  const sc = $("stream");
  sc.scrollTop = sc.scrollHeight;
}
let busy = false, again = false;
async function refresh(force) {
  if (busy) { if (force) again = true; return; }
  busy = true;
  try {
    S.st = await api("/api/state");
    $("app").hidden = false; $("authfail").hidden = true;
    if (!room()) switchTo((S.st.rooms.find((r) => r.id === "local") || S.st.rooms[0] || {}).id || null);
    const gen = S.gen, rid = S.room;
    if (rid) {
      const msgs = await loadHistory(rid);
      if (gen !== S.gen) { again = true; return; } // room changed while loading: drop it
      S.msgs = msgs; S.shown = rid;
      if (S.msgs.length < 100 && !S.older.length) S.more = false;
      // Mark seen only what is actually in the loaded history for this room;
      // unanswered questions stay until answered.
      const have = new Set(S.msgs.concat(S.older).map((m) => m.id));
      const ids = unreadFor(rid).filter((m) => !isQuestion(m) && have.has(m.id)).map((m) => m.id);
      if (ids.length) {
        await api("/api/seen", { ids });
        if (gen !== S.gen) { again = true; return; }
        S.st.unread = S.st.unread.filter((m) => !ids.includes(m.id));
      }
      await updateSummaries();
      if (gen !== S.gen) { again = true; return; }
    }
    renderSide(); renderNeeds(); renderHead(); renderStream(force);
  } catch (e) { if (e.message !== "unauthorized") $("err").textContent = e.message; }
  finally {
    busy = false;
    if (again) { again = false; refresh(true); }
  }
}

$("older").addEventListener("click", async () => {
  const first = all()[0];
  if (!first || S.shown !== S.room) return;
  const gen = S.gen, rid = S.room;
  try {
    const page = await loadHistory(rid, "&before=" + first.n);
    if (gen !== S.gen) return;
    S.older = page.concat(S.older);
    if (page.length < 100) S.more = false;
    renderStream(true);
  } catch (e) { $("err").textContent = e.message; }
});
$("traceclose").addEventListener("click", () => $("trace").close());

async function submit() {
  const ta = $("text"), text = ta.value.trim();
  // Send to the room whose history is on screen, and only if it is the selected one.
  if (!text || !S.room || S.shown !== S.room) return;
  const dest = S.shown, gen = S.gen;
  const body = { room: dest, text };
  if ($("to").value) body.to = [$("to").value];
  if (S.reply) body.reply_to = S.reply.id;
  try {
    await api("/api/send", body);
    ta.value = "";
    if (gen !== S.gen) { refresh(true); return; } // room changed mid-send; the new room was cleared already
    S.reply = null; $("replying").hidden = true; $("err").textContent = "";
    await refresh(true);
    $("stream").scrollTop = $("stream").scrollHeight;
  } catch (e) { $("err").textContent = e.message; }
}
$("compose").addEventListener("submit", (e) => { e.preventDefault(); submit(); });
$("text").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); submit(); }
});

if (!token) showAuthFail();
else { refresh(true); setInterval(refresh, 2000); }
