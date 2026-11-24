"use strict";
// All peer-supplied text is rendered with textContent only; never markup.
const $ = (id) => document.getElementById(id);
// notice: the one-time upgrade notice, kept for the life of the page because
// the server returns it only once. onscreen: key -> version of every entry as
// rendered; only that is ever confirmed. qShown: the same for question cards.
const S = { st: null, unread: [], questions: [], qKeys: new Set(), waiting: 0, alerts: [], requests: [], room: null, msgs: [], older: [], notes: [], more: true, reply: null, sigs: {}, toSig: "", gen: 0, shown: null, toRoom: null, outcome: "", notice: "", onscreen: new Map(), qShown: new Map() };
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
// Badges count ordinary mail only: questions have their own section.
const unreadFor = (id) => S.unread.filter((m) => m.room === id && !S.qKeys.has(m.key));
const all = () => S.older.concat(S.msgs);

const NETWORK_WORDS = {
  off: "off (this machine only)",
  outbound: "outbound only (this machine can reach others, others cannot reach it)",
  on: "on (other machines can reach this one)",
};

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
  const net = S.st.network || { state: listen.length ? "on" : "off" };
  box.append(el("h2", "", "This node"),
    el("div", "", el("strong", "", S.st.name), " ", el("span", "muted", fp(S.st.node))),
    el("div", "", el("span", "dot " + (net.state === "off" ? "" : "ok")), "Network: " + (NETWORK_WORDS[net.state] || net.state)));
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
  if (S.st.notice) S.notice = S.st.notice; // later polls return none; keep it
  const n = $("notice");
  if (n.dataset.t === S.notice) return;
  n.dataset.t = S.notice;
  n.replaceChildren();
  n.hidden = !S.notice;
  if (S.notice) n.append(el("span", "", S.notice));
}
const field = (k, v, cls) => el("div", "field", el("span", "k", k), el("span", "v" + (cls ? " " + cls : ""), v));
function requestCard(r, row) {
  const c = el("div", "qcard");
  c.append(el("div", "sub", "Request " + fp(r.id) + " (" + r.kind + ")"));
  if (r.kind === "network") {
    c.append(field("Networking", r.action), field("Listen address", r.name || "none"));
  } else {
    c.append(field("Action", r.action), field("Room", (r.room_label || "") + " (" + r.room + ")"),
      field("Machine fingerprint", r.fingerprint, "mono"),
      field("Name", (r.name || "none") + " (chosen by that machine)"));
    if (r.role) c.append(field("Role", r.role));
    if (r.action !== "remove" && r.action !== "promote") c.append(field("History", r.history ? "may read earlier history" : "no earlier history"));
    c.append(field("Seconding", r.seconding ? "yes" : "no"));
    c.append(field("Invite deadline", r.deadline && !String(r.deadline).startsWith("0001") ? fmt(r.deadline) || r.deadline : "none"));
  }
  c.append(el("div", "txt", r.text), row);
  return c;
}
function renderNeeds() {
  const qs = S.questions;
  const alerts = S.alerts.filter((a) => a.kind === "fork" || a.kind === "missing");
  const sig = JSON.stringify([S.requests, alerts.map((a) => [a.id, a.text]), qs.map((m) => [m.key, m.version, m.text]), S.outcome]);
  $("needs").hidden = !(qs.length || alerts.length || S.requests.length || S.outcome);
  if (sig === S.sigs.n) return;
  S.sigs.n = sig;
  $("outcome").textContent = S.outcome;
  const rq = $("requests");
  rq.replaceChildren();
  for (const r of S.requests) {
    const row = el("div", "row");
    row.append(btn("Approve this", "pri", () => decide(r, true, row), r.text), btn("Decline this", "", () => decide(r, false, row), r.text));
    rq.append(requestCard(r, row));
  }
  const al = $("alerts");
  al.replaceChildren();
  for (const a of alerts) al.append(el("div", "warnline", a.kind + " in " + a.room + ": " + a.text + (a.since ? " (since " + fmt(a.since) + ")" : "")));
  const box = $("questions");
  box.replaceChildren();
  S.qShown = new Map();
  for (const m of qs) {
    S.qShown.set(m.key, m.version);
    const input = el("input");
    input.placeholder = "Type an answer";
    const answer = (t) => t.trim() && sendAnswer(m, t.trim());
    input.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); answer(input.value); } });
    const row = el("div", "row");
    for (const c of m.choices || []) row.append(btn(c, "pri", () => answer(c)));
    row.append(input, btn("Answer", "", () => answer(input.value)), btn("Dismiss", "link", () => dismissQuestion(m), "Mark read without answering"));
    box.append(el("div", "qcard", el("div", "sub", m.from + " asks (" + (m.room || "") + ")"), el("div", "txt", m.text || ""), row));
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
// confirmSeen confirms exactly the key and version given and returns the set
// of keys the server confirmed; a refused entry stays unread.
async function confirmSeen(entries) {
  const ok = new Set();
  if (!entries.length) return ok;
  const r = await api("/api/seen", { entries: entries.map((m) => ({ key: m.key, version: m.version })) });
  const asked = new Set(entries.map((m) => m.key));
  for (const x of (r && r.results) || []) if (x.ok && asked.has(x.key)) ok.add(x.key);
  S.unread = S.unread.filter((m) => !ok.has(m.key));
  S.questions = S.questions.filter((m) => !ok.has(m.key));
  return ok;
}
// A question is confirmed only for the version the card showed.
const questionOnScreen = (m) => S.qShown.get(m.key) === m.version;
async function finishQuestion(m) {
  if (!questionOnScreen(m)) throw new Error("the question changed; read the new version first");
  const ok = await confirmSeen([m]); // handling may be reported only for text confirmed seen
  if (!ok.has(m.key)) throw new Error("could not confirm the question as read; it stays listed");
  await api("/api/ack", { id: m.id });
  S.sigs.n = "";
  refresh(true);
}
async function sendAnswer(m, text) {
  try {
    if (!questionOnScreen(m)) throw new Error("the question changed; read the new version first");
    await api("/api/send", { room: m.room, text, reply_to: m.id });
    await finishQuestion(m);
  } catch (e) { $("err").textContent = e.message; refresh(true); }
}
async function dismissQuestion(m) {
  try { await finishQuestion(m); } catch (e) { $("err").textContent = e.message; refresh(true); }
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
  const sig = S.room + "|" + list.map((m) => m.id + "@" + m.version).join() + "|" + notes.map((m) => m.key + "@" + m.version).join() + "|" + S.more;
  if (sig === S.sigs.s && !force) return;
  S.sigs.s = sig;
  S.onscreen = new Map(list.map((m) => [m.key, m.version]));
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
    S.questions = d.questions || []; S.qKeys = new Set(S.questions.map((m) => m.key));
    $("app").hidden = false; $("authfail").hidden = true;
    if (!room()) switchTo((rooms().find((r) => r.id === S.st.local) || rooms()[0] || {}).id || null);
    const gen = S.gen, rid = S.room;
    renderNotice(); renderNeeds();
    if (rid) {
      const h = await loadHistory(rid);
      if (gen !== S.gen) { again = true; return; } // room changed while loading: drop it
      reconcile(h.entries || [], rid);
      S.shown = rid;
      // Ordinary mail of this room: notices are kept for display, and
      // everything is confirmed only as rendered. Questions are never here.
      const ord = S.unread.filter((m) => m.room === rid && !S.qKeys.has(m.key));
      for (const n of ord) {
        if (n.kind === "message") continue;
        const i = S.notes.findIndex((x) => x.key === n.key);
        if (i < 0) S.notes.push(n); else S.notes[i] = n;
      }
      renderStream(force);
      const mine = ord.filter((m) => m.kind === "message"
        ? S.onscreen.get(m.key) === m.version
        : S.notes.some((x) => x.key === m.key && x.version === m.version));
      if (mine.length) {
        await confirmSeen(mine);
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
// reconcile makes the newest page authoritative and repairs the cached older
// pages: an entry the newest page now holds is dropped from them, and the cache
// is discarded (and reloaded on demand) when it may have a gap or hold text
// that was replaced or withdrawn.
function reconcile(page, rid) {
  const ids = new Set(page.map((m) => m.id));
  const prev = new Set(S.msgs.map((m) => m.id));
  let older = S.older.filter((m) => !ids.has(m.id));
  const touched = S.unread.some((u) => u.room === rid && older.some((o) => o.id === u.id));
  const gap = prev.size > 0 && page.length >= 100 && !page.some((m) => prev.has(m.id));
  if (page.length < 100) { older = []; S.more = false; } // the page holds the whole history
  else if (touched || gap) { older = []; S.more = true; }
  S.older = older;
  S.msgs = page;
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
