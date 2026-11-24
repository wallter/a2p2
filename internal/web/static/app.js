"use strict";
// All peer-supplied text is rendered with textContent only; never markup.
const $ = (id) => document.getElementById(id);
const S = { st: null, unread: [], waiting: 0, alerts: [], requests: [], room: null, msgs: [], older: [], more: true, reply: null, sigs: {}, toSig: "", gen: 0, shown: null, toRoom: null, outcome: "", noticeSeen: "", noticeShown: "" };
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
function btn(label, cls, fn, title) {
  const b = el("button", cls, label);
  b.type = "button";
  if (title) b.title = title;
  b.addEventListener("click", (ev) => { ev.stopPropagation(); fn(ev); });
  return b;
}
const fp = (id) => String(id || "").slice(0, 8);
function fmt(t) {
  const d = new Date(t), now = new Date();
  if (isNaN(d)) return "";
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

const rooms = () => (S.st && S.st.rooms) || [];
const room = () => rooms().find((r) => r.id === S.room) || null;
const unreadFor = (id) => S.unread.filter((m) => m.room === id);
const isQuestion = (m) => m.kind === "message" && (m.to || []).includes(S.st.human) &&
  ((m.choices && m.choices.length > 0) || String(m.text || "").trim().endsWith("?"));
const all = () => S.older.concat(S.msgs);

// ---- sidebar ----
function renderSide() {
  const ul = $("rooms");
  ul.replaceChildren();
  for (const r of rooms()) {
    const n = unreadFor(r.id).length, nm = (r.members || []).length;
    const admin = (r.admins || []).includes(S.st.node);
    const li = el("li", r.id === S.room ? "sel" : "");
    li.append(el("span", "l", r.label, el("div", "sub", nm + (nm === 1 ? " node" : " nodes") + " · " + (admin ? "admin" : "member"))));
    if (n) li.append(el("span", "badge", n));
    li.addEventListener("click", () => openRoom(r.id));
    ul.append(li);
  }
  const box = $("node");
  box.replaceChildren();
  const r = room();
  if (r) {
    box.append(el("h2", "", "Members of " + r.label));
    for (const m of r.members || []) {
      const adm = m.admin || (r.admins || []).includes(m.node);
      box.append(el("div", "mem", m.name || fp(m.node), " ", el("span", "muted", fp(m.node)), " ", adm ? el("span", "mark adm", "admin") : null));
    }
    for (const p of r.pending || []) box.append(el("div", "sub", "pending " + p.kind + " " + p.state + (p.reason ? ": " + p.reason : "")));
    for (const l of r.limits || []) box.append(el("div", "sub", l));
  }
  const listen = S.st.listen || [];
  box.append(el("h2", "", "This node"),
    el("div", "", el("strong", "", S.st.name), " ", el("span", "muted", fp(S.st.node))),
    el("div", "", el("span", "dot " + (listen.length ? "ok" : "")), "Network " + (listen.length ? "on" : "off (this machine only)")));
  for (const a of listen) box.append(el("div", "sub", a));
  box.append(el("h2", "", "Peers"));
  if (!(S.st.peers || []).length) box.append(el("div", "sub", "No peers yet."));
  for (const p of S.st.peers || []) {
    box.append(el("div", "", p.name || fp(p.node), " ", el("span", "muted", fp(p.node))),
      el("div", "sub", p.last_heard ? "heard " + ago(p.last_heard) : "not heard from yet"));
  }
  box.append(el("h2", "", "Local agents"));
  for (const a of S.st.agents || []) box.append(el("div", "", a.label || a.id, " ", el("span", "sub", a.harness + " · " + ago(a.last_seen))));
}

// ---- banner and needs you ----
function renderNotice() {
  const t = S.st.notice || "";
  if (t && t !== S.noticeSeen) S.noticeShown = t;
  const n = $("notice"), cur = S.noticeShown;
  if (n.dataset.t === cur) return;
  n.dataset.t = cur;
  n.replaceChildren();
  n.hidden = !cur;
  if (cur) n.append(el("span", "", cur), btn("Dismiss", "", () => { S.noticeSeen = cur; S.noticeShown = ""; renderNotice(); }));
}
function renderNeeds() {
  const qs = S.unread.filter(isQuestion);
  const alerts = S.alerts.filter((a) => a.kind === "fork" || a.kind === "missing");
  const sig = JSON.stringify([S.requests.map((r) => [r.id, r.text]), alerts.map((a) => [a.id, a.text]), qs.map((m) => m.key), S.outcome]);
  $("needs").hidden = !(qs.length || alerts.length || S.requests.length || S.outcome);
  if (sig === S.sigs.n) return;
  S.sigs.n = sig;
  $("outcome").textContent = S.outcome;
  const rq = $("requests");
  rq.replaceChildren();
  for (const r of S.requests) {
    const row = el("div", "row");
    row.append(btn("Approve this", "pri", () => decide(r, true, row), r.text), btn("Decline this", "", () => decide(r, false, row), r.text));
    rq.append(el("div", "qcard", el("div", "sub", (r.name || fp(r.node)) + " asks (" + r.kind + ", room " + r.room + (r.role ? ", role " + r.role : "") + ")"),
      el("div", "txt", r.text), row));
  }
  const al = $("alerts");
  al.replaceChildren();
  for (const a of alerts) al.append(el("div", "warnline", a.kind + " in " + a.room + ": " + a.text + (a.since ? " (since " + fmt(a.since) + ")" : "")));
  const box = $("questions");
  box.replaceChildren();
  for (const m of qs) {
    const input = el("input");
    input.placeholder = "Type an answer";
    const answer = (t) => t.trim() && sendAnswer(m, t.trim());
    input.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); answer(input.value); } });
    const row = el("div", "row");
    for (const c of m.choices || []) row.append(btn(c, "pri", () => answer(c)));
    row.append(input, btn("Answer", "", () => answer(input.value)));
    box.append(el("div", "qcard", el("div", "sub", m.from + " asks"), el("div", "txt", m.text || ""), row));
  }
}
async function decide(r, approve, row) {
  for (const b of row.querySelectorAll("button")) b.disabled = true;
  try {
    const o = await api("/api/confirm", { id: r.id, approve, text: r.text });
    S.outcome = "Request " + fp(r.id) + ": " + o.state + (o.reason ? " (" + o.reason + ")" : "");
  } catch (e) { S.outcome = "Request " + fp(r.id) + " failed: " + e.message; }
  S.sigs.n = "";
  refresh(true);
}
async function markSeen(entries) {
  if (!entries.length) return;
  await api("/api/seen", { entries: entries.map((m) => ({ key: m.key, version: m.version })) });
  const keys = new Set(entries.map((m) => m.key));
  S.unread = S.unread.filter((m) => !keys.has(m.key));
}
async function sendAnswer(m, text) {
  try {
    await api("/api/send", { room: m.room, text, reply_to: m.id });
    await markSeen([m]); // handling may be reported only for text confirmed seen
    await api("/api/ack", { id: m.id });
    refresh(true);
  } catch (e) { $("err").textContent = e.message; }
}

// ---- stream ----
function entryMeta(m) {
  const meta = el("div", "meta", el("span", "from", m.from), el("span", "", fmt(m.created)));
  if (m.to && m.to.length) meta.append(el("span", "", "to " + m.to.join(", ")));
  if (m.parent) meta.append(el("span", "", "reply to " + fp(m.parent) + (m.parent_state && m.parent_state !== "here" ? " (" + m.parent_state + ")" : "")));
  if (m.author_removed) meta.append(el("span", "mark", "removed machine"));
  if (m.late) meta.append(el("span", "mark", "late"));
  return meta;
}
// Notices stay in S.notes until the room is left, so they remain visible
// after being confirmed.
function renderStream(force) {
  const list = all().filter((m) => m.text);
  const notes = S.notes;
  const sig = S.room + "|" + list.map((m) => m.id).join() + "|" + notes.map((m) => m.key).join() + "|" + S.more;
  if (sig === S.sigs.s && !force) return;
  S.sigs.s = sig;
  const sc = $("stream");
  const atBottom = sc.scrollHeight - sc.scrollTop - sc.clientHeight < 40;
  const box = $("msgs");
  box.replaceChildren();
  for (const m of list) {
    const meta = entryMeta(m);
    meta.append(btn("reply", "link", () => setReply(m)), btn("thread", "link", () => openThread(m.thread || m.id)));
    const d = el("div", "msg" + (m.from === S.st.human ? " mine" : ""), meta, el("div", "body", m.text));
    d.addEventListener("click", () => openTrace(m.id));
    box.append(d);
  }
  for (const n of notes) box.append(el("div", "noteline", n.kind + ": " + fmt(n.created) + ", message " + fp(n.id)));
  $("older").hidden = !S.more || !S.msgs.length;
  $("empty").hidden = !(!(S.st.agents || []).some((a) => a.id !== S.st.human) && !list.length);
  if (atBottom) sc.scrollTop = sc.scrollHeight;
}
function updateCompose() {
  const r = room(), ready = !!r && S.shown === S.room;
  $("text").placeholder = r ? "Message #" + r.label + ". Enter sends, Shift+Enter adds a line." : "Select a room";
  const b = $("compose").querySelector("button[type=submit]");
  if (b) { b.disabled = !ready; b.textContent = r ? "Send to #" + r.label : "Send"; }
}
function renderHead() {
  const r = room();
  $("roomhead").textContent = r ? r.label + (S.waiting ? " (fetching " + S.waiting + " more)" : "") : "";
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
  r.replaceChildren("Replying to " + m.from + ": " + (m.text || "").slice(0, 60) + " ", btn("cancel", "link", () => { S.reply = null; r.hidden = true; }));
  $("text").focus();
}

// ---- dialogs ----
function showDialog(...kids) {
  $("tracebody").replaceChildren(...kids);
  if (!$("trace").open) $("trace").showModal();
}
async function openTrace(id) {
  try {
    const t = await api("/api/trace?id=" + encodeURIComponent(id));
    const ct = el("table"), ht = el("table");
    ct.append(el("tr", "", ...["Node", "When", "Member"].map((h) => el("th", "", h))));
    for (const c of t.copies || []) ct.append(el("tr", "", el("td", "", fp(c.node)), el("td", "", fmt(c.at)), el("td", "", c.member ? "yes" : "no")));
    ht.append(el("tr", "", ...["Agent", "When"].map((h) => el("th", "", h))));
    for (const h of t.handled || []) ht.append(el("tr", "", el("td", "", h.agent), el("td", "", fmt(h.at))));
    showDialog(el("h2", "", "Message " + fp(id)),
      el("div", "", "Saved: " + (t.saved ? "yes" : "no") + " (" + t.reason + ")"),
      el("h2", "", "Copies"), (t.copies || []).length ? ct : el("div", "sub", "No copies recorded."),
      el("h2", "", "Handled"), (t.handled || []).length ? ht : el("div", "sub", "Nobody has handled it yet."));
  } catch (e) { $("err").textContent = e.message; }
}
async function openThread(id) {
  try {
    const t = await api("/api/thread?id=" + encodeURIComponent(id));
    const kids = [el("h2", "", "Thread " + fp(id))];
    for (const m of t.entries || []) kids.push(el("div", "thr", entryMeta(m), el("div", "body", m.text || m.kind)));
    if (kids.length === 1) kids.push(el("div", "sub", "Nothing in this thread."));
    showDialog(...kids);
  } catch (e) { $("err").textContent = e.message; }
}

// ---- data flow ----
const loadHistory = (rid, extra) => api("/api/history?room=" + encodeURIComponent(rid) + "&limit=100" + (extra || ""));
// switchTo makes id the current room and discards everything shown for the
// previous one; responses still in flight for it are dropped by generation.
function switchTo(id) {
  S.gen++; S.room = id; S.shown = null; S.msgs = []; S.older = []; S.notes = []; S.more = true; S.reply = null; S.toSig = "";
  S.sigs.s = "";
  $("msgs").replaceChildren();
  $("replying").hidden = true;
  $("older").hidden = true;
  if (S.st) { renderHead(); renderSide(); }
}
async function openRoom(id) {
  if (id === S.room && S.shown === id) return;
  switchTo(id);
  await refresh(true);
  $("stream").scrollTop = $("stream").scrollHeight;
}
let busy = false, again = false;
async function refresh(force) {
  if (busy) { if (force) again = true; return; }
  busy = true;
  try {
    const d = await api("/api/state");
    S.st = d.status; S.unread = d.unread || []; S.waiting = d.waiting || 0; S.alerts = d.alerts || []; S.requests = d.requests || [];
    $("app").hidden = false; $("authfail").hidden = true;
    if (!room()) switchTo((rooms().find((r) => r.id === S.st.local) || rooms()[0] || {}).id || null);
    const gen = S.gen, rid = S.room;
    if (rid) {
      const h = await loadHistory(rid);
      if (gen !== S.gen) { again = true; return; } // room changed while loading: drop it
      S.msgs = h.entries || []; S.shown = rid;
      if (S.msgs.length < 100 && !S.older.length) S.more = false;
      // Notices of this room are kept for display, then confirmed with the
      // messages that are on screen; unanswered questions stay until answered.
      const have = new Set(all().map((m) => m.id));
      const mine = unreadFor(rid).filter((m) => !isQuestion(m) && (m.kind !== "message" || have.has(m.id)));
      for (const n of mine) if (n.kind !== "message" && !S.notes.some((x) => x.key === n.key)) S.notes.push(n);
      renderStream(force);
      if (mine.length) {
        await markSeen(mine);
        if (gen !== S.gen) { again = true; return; }
      }
    }
    renderNotice(); renderSide(); renderNeeds(); renderHead(); renderStream(force);
  } catch (e) { if (e.message !== "unauthorized") $("err").textContent = e.message; }
  finally {
    busy = false;
    if (again) { again = false; refresh(true); }
  }
}

$("older").addEventListener("click", async () => {
  const first = all().find((m) => m.id);
  if (!first || S.shown !== S.room) return;
  const gen = S.gen, rid = S.room;
  try {
    const page = ((await loadHistory(rid, "&before=" + encodeURIComponent(first.id))).entries) || [];
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
