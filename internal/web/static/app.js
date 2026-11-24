"use strict";
// All peer-supplied text is rendered with textContent only; never markup.
const $ = (id) => document.getElementById(id);
// notice: the one-time upgrade notice, kept for the life of the page because
// the server returns it only once. onscreen: key -> version of every entry as
// rendered; only that is ever confirmed. qShown: the same for question cards.
const S = { st: null, unread: [], questions: [], qKeys: new Set(), waiting: 0, alerts: [], requests: [], room: null, msgs: [], older: [], notes: [], more: true, reply: null, sigs: {}, toSig: "", gen: 0, shown: null, toRoom: null, outcome: "", notice: "", risky: new Set(), onscreen: new Map(), qShown: new Map(), notesShown: new Map(), boundary: "" };
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
// escapeDisplay writes the characters that can hide or reorder what a
// person reads as \u{...}, as node.EscapeDisplay does (PROTOCOL 10.8).
function escapeDisplay(s) {
  let out = "";
  for (const ch of String(s == null ? "" : s)) {
    const c = ch.codePointAt(0);
    const hides = c < 0x20 || (c >= 0x7f && c <= 0x9f) || c === 0x61c || c === 0x200e || c === 0x200f ||
      (c >= 0x202a && c <= 0x202e) || (c >= 0x2066 && c <= 0x2069);
    out += hides ? "\\u{" + c.toString(16) + "}" : ch;
  }
  return out;
}
// sizeText is cmd/a2p2 sizeText: bytes, with a rounded size above 1 KiB.
function sizeText(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + " bytes";
  const units = ["KiB", "MiB", "GiB"];
  let v = n / 1024, u = 0;
  while (v >= 1024 && u < units.length - 1) { v /= 1024; u++; }
  return v.toFixed(1) + " " + units[u] + " (" + n + " bytes)";
}
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
function showAuthFail() { $("connection").hidden = true; $("app").hidden = true; $("authfail").hidden = false; }

const rooms = () => (S.st && S.st.rooms) || [];
const room = () => rooms().find((r) => r.id === S.room) || null;
function roomLabel(id) {
  const entries = rooms();
  const r = entries.find((r) => r.id === id);
  if (!r) return id;
  return entries.some((other) => other.id !== id && other.label === r.label)
    ? r.label + " (" + id + ")" : r.label;
}
// Badges count ordinary mail only: questions have their own section.
const unreadFor = (id) => S.unread.filter((m) => m.room === id && !S.qKeys.has(m.key));
const all = () => S.older.concat(S.msgs);

const NETWORK_WORDS = {
  off: "off (this machine only)",
  outbound: "outbound only (this machine can reach others, others cannot reach it)",
  on: "on (accepts incoming connections)",
};

// ---- sidebar ----
function renderSide() {
  const ul = $("rooms");
  const choices = rooms().map((r) => [r.id, roomLabel(r.id), unreadFor(r.id).length,
    (r.members || []).length, !!r.may_be_removed]);
  const sig = JSON.stringify([S.room, choices]);
  if (sig !== S.sigs.rooms) {
    S.sigs.rooms = sig;
    const focused = ul.contains(document.activeElement) ? document.activeElement.dataset.room : null;
    ul.replaceChildren();
    for (const [id, label, unread, count, removed] of choices) {
      const b = btn("", "room-choice", () => openRoom(id));
      b.dataset.room = id;
      if (id === S.room) b.setAttribute("aria-current", "true");
      b.append(el("span", "l", label, el("span", "sub" + (removed ? " warn" : ""),
        removed ? REMOVED : count === 1 ? "this machine only" : count + " machines")));
      if (unread) {
        const badge = el("span", "badge", unread);
        badge.setAttribute("aria-label", unread + " unread");
        b.append(badge);
      }
      ul.append(el("li", id === S.room ? "sel" : "", b));
      if (id === focused) b.focus({ preventScroll: true });
    }
  }
  const box = $("node");
  const limits = box.querySelector("details");
  const r = room();
  const keepOpen = limits && limits.open && box.dataset.room === S.room;
  const refocus = limits && limits.contains(document.activeElement);
  box.dataset.room = S.room;
  box.replaceChildren();
  if (r) {
    box.append(el("h2", "", "Members of " + r.label));
    for (const m of r.members || []) {
      const adm = m.admin || (r.admins || []).includes(m.node);
      // A machine with no name (a room's creator, seen from elsewhere) shows its short id once.
      const shared = (r.members || []).length > 1;
      box.append(el("div", "mem", m.name || fp(m.node), m.name ? " " : null, m.name ? el("span", "muted", fp(m.node)) : null,
        " ", adm && shared ? el("span", "mark adm", "admin") : null));
    }
    for (const p of r.pending || []) box.append(el("div", "sub", "pending " + p.kind + " " + p.state + (p.reason ? ": " + p.reason : "")));
    if (r.limits && r.limits.length && r.id !== S.st.local) { // the local room is never shared
      const details = el("details", "", el("summary", "", "Room limits"), ...r.limits.map((l) => el("p", "sub", l)));
      details.open = !!keepOpen;
      box.append(details);
      if (refocus) details.firstChild.focus({ preventScroll: true });
    }
  }
  const listen = S.st.listen || [];
  const net = S.st.network || { state: listen.length ? "on" : "off" };
  box.append(el("h2", "", "This machine"),
    el("div", "", el("strong", "", S.st.name), " ", el("span", "muted", fp(S.st.node))),
    el("div", "", el("span", "dot " + (net.state === "off" ? "" : "ok")), "Network: " + (NETWORK_WORDS[net.state] || net.state)));
  if (net.state !== "off" && S.st.reach) {
    const rc = S.st.reach;
    box.append(el("div", "sub", "Reachable by peers: " + rc.state + (rc.checked ? ", checked " + ago(rc.checked) : "") +
      (rc.why ? " (" + rc.why + ")" : "")));
  }
  if (S.st.files) box.append(el("div", "sub", "Files: " + sizeText(S.st.files.held) + " of " + sizeText(S.st.files.budget) + " held"));
  for (const a of listen) {
    const loopback = /^(127\.|\[::1\]:|localhost:)/i.test(a);
    box.append(el("div", "sub", a + (loopback ? " (this machine only)" : "")));
  }
  box.append(el("h2", "", "Other machines"));
  if (!(S.st.peers || []).length) box.append(el("div", "sub", "No other machines yet."));
  for (const p of S.st.peers || []) {
    box.append(el("div", "", p.name || fp(p.node), p.name ? " " : null, p.name ? el("span", "muted", fp(p.node)) : null),
      el("div", "sub", p.last_heard ? "heard " + ago(p.last_heard) : "not heard from yet"));
  }
  box.append(el("h2", "", "Local agents"));
  // Each agent's reading level per room (U1): only levels other than "all"
  // are listed. The person reads every room in full here, whatever any
  // agent's level.
  const roomLabel = (id) => ((S.st.rooms || []).find((r) => r.id === id) || {}).label || id;
  for (const a of S.st.agents || []) {
    box.append(el("div", "", a.label || a.id, " ", el("span", "sub", a.harness + " · " + ago(a.last_seen))));
    if (a.id === S.st.human) continue;
    const ls = (S.st.levels || []).filter((l) => l.agent === a.id);
    const words = ls.map((l) => roomLabel(l.room) + ": " + l.level);
    box.append(el("div", "sub", "level " + (words.length ? words.join(", ") + "; other rooms: all" : "all in every room")));
  }
}

// The room's files setting (PROTOCOL 10.6, room/files): a person's
// decision. It is drawn again only when it changes, and not while its
// control has focus, so a poll does not take the choice away.
const FILE_MODES = {
  off: "off: does not fetch a file it lacks for a member who asks",
  carry: "carry: fetches a file a member asks it for from the author and holds it while the text lives (the default)",
  keep: "keep: fetches every file of the room",
};
let filesNote = "";
function renderRoomFiles() {
  const box = $("roomfiles"), r = room(), f = r && r.files;
  if (box.contains(document.activeElement) && box.dataset.room === S.room) return;
  const sig = JSON.stringify([S.room, f, filesNote]);
  if (sig === S.sigs.files) return;
  S.sigs.files = sig;
  box.dataset.room = S.room;
  box.replaceChildren();
  if (!f) return;
  box.append(el("h2", "", "Files in " + r.label),
    el("div", "", "Setting: " + (FILE_MODES[f.mode] || f.mode)),
    el("div", "sub", sizeText(f.held) + " of " + sizeText(f.budget) + " held for this room"));
  const sel = el("select");
  sel.id = "files-mode";
  for (const m of ["off", "carry", "keep"]) { const o = el("option", "", m); o.value = m; sel.append(o); }
  sel.value = f.mode;
  const label = el("label", "", "Change setting");
  label.htmlFor = "files-mode";
  const rid = r.id;
  box.append(el("div", "row", label, sel, btn("Set files setting", "", () => setRoomFiles(rid, sel.value))));
  if (filesNote) box.append(el("div", "sub", filesNote));
}
async function setRoomFiles(rid, mode) {
  try {
    const v = await api("/api/room-files", { room: rid, mode });
    filesNote = "Files setting is now " + v.mode + ".";
  } catch (e) { filesNote = "Could not change the files setting: " + e.message; }
  S.sigs.files = "";
  if (document.activeElement) document.activeElement.blur();
  await refresh(true);
}

// ---- banner and needs you ----
// The notice is reported shown only once it is drawn; it stays on screen
// for the life of the page, since the server stops returning it then.
let noticeAcked = false;
function renderNotice() {
  if (S.st.notice) S.notice = S.st.notice;
  const n = $("notice");
  if (n.dataset.t !== S.notice) {
    n.dataset.t = S.notice;
    n.replaceChildren();
    n.hidden = !S.notice;
    if (S.notice) n.append(el("span", "", S.notice));
  }
  if (S.notice && !noticeAcked && !n.hidden && n.isConnected) {
    noticeAcked = true;
    requestAnimationFrame(() => api("/api/notice-shown", {}).catch(() => { noticeAcked = false; }));
  }
}
function requestCard(r, row) {
  // These are the exact terms compared by the daemon when the person decides.
  const c = el("div", "qcard", el("div", "sub", "Request " + fp(r.id)), el("div", "txt", r.text));
  const due = r.deadline && !String(r.deadline).startsWith("0001") ? (fmt(r.deadline) || r.deadline) : "";
  if (r.kind === "send_files") {
    c.append(sendTerms(r));
    if (due) c.append(el("div", "sub", "Expires " + due + ", declined unless approved"));
  } else {
    if (r.name && r.kind !== "network") c.append(el("div", "sub", "The machine chose its own name."));
    if (due) c.append(el("div", "sub", "Invite expires " + due));
  }
  c.append(row);
  return c;
}
// sendTerms shows every term of a send with files, as a2p2 confirm does
// (cmd/a2p2 printSendTerms). The daemon has escaped controls and
// bidirectional characters in paths, names and lines; the room's label is
// escaped here the same way.
function sendTerms(r) {
  const t = el("dl", "terms");
  const row = (k, ...v) => t.append(el("dt", "", k), el("dd", "", ...v));
  row("agent", r.agent || "");
  row("room", escapeDisplay(r.room_label) + " (" + r.room + ")");
  const to = r.to || [];
  if (to.length === 1 && to[0] === "everyone") row("to", "everyone in the room (" + (r.members || 0) + " members)");
  else if (to.length) row("to", to.join(", "));
  else if (r.reply_to) row("to", "the author of message " + r.reply_to);
  if (r.replaces) row("replaces", r.replaces.id + ": \"" + r.replaces.line + "\"",
    el("div", "sub", "the replacement has only the files listed here"));
  row("text", "\"" + (r.text_line || "") + "\"");
  (r.files || []).forEach((f, i) => row("file " + (i + 1), el("div", "path", f.path),
    el("div", "sub", f.name + ", " + sizeText(f.size) + ", sha256 " + f.sha256 + (f.kept ? ", kept from the message replaced" : ""))));
  row("terms", r.terms || "");
  return t;
}
function renderNeeds() {
  const qs = S.questions;
  const alerts = S.alerts.filter((a) => a.kind === "fork" || a.kind === "missing");
  const sig = JSON.stringify([
    S.requests, alerts.map((a) => [a.id, roomLabel(a.room), a.text, a.since]),
    qs.map((m) => [m.key, m.version, roomLabel(m.room)]), S.outcome, [...S.risky],
  ]);
  $("needs").hidden = !(qs.length || alerts.length || S.requests.length || S.outcome);
  if (sig === S.sigs.n) return;
  S.sigs.n = sig;
  const count = qs.length + alerts.length + S.requests.length;
  $("needs-title").textContent = count ? "Needs you (" + count + ")" : "Update";
  if ($("outcome").textContent !== S.outcome) $("outcome").textContent = S.outcome;
  const rq = $("requests");
  rq.replaceChildren();
  for (const r of S.requests) {
    const row = el("div", "row");
    if (S.risky.has(r.id)) {
      // The first approval answered not_caught_up: name the risk and take a
      // second, explicit approval (sent as not_caught_up_ok).
      row.append(el("div", "warnline", NOT_CAUGHT_UP),
        btn("Approve anyway, I accept this risk", "pri", () => decide(r, true, row, true), r.text),
        btn("Decline request", "", () => decide(r, false, row)));
    } else {
      const remove = r.action === "remove";
      const label = remove ? "Approve removal" : r.kind === "send_files" ? "Approve send with files" : "Approve request";
      row.append(
        btn(label, remove ? "danger" : "pri", () => decide(r, true, row)),
        btn("Decline request", "", () => decide(r, false, row))
      );
    }
    rq.append(requestCard(r, row));
  }
  const al = $("alerts");
  al.replaceChildren();
  for (const a of alerts) al.append(el("div", "warnline", a.kind + " in " + roomLabel(a.room) + ": " + a.text + (a.since ? " (since " + fmt(a.since) + ")" : "")));
  const box = $("questions");
  // Keep a draft and its focused control when unrelated alerts or mail arrive.
  const existing = new Map(Array.from(box.children, (card) => [card.dataset.key, card]));
  S.qShown = new Map();
  for (const m of qs) {
    S.qShown.set(m.key, m.version);
    const prev = existing.get(m.key);
    existing.delete(m.key);
    if (prev && prev.dataset.version === m.version) {
      prev.firstChild.textContent = m.from + " in " + roomLabel(m.room);
      continue;
    }
    const input = el("input");
    input.placeholder = "Write a reply";
    input.setAttribute("aria-label", "Reply to " + m.from + " in " + roomLabel(m.room));
    const error = el("div", "question-error");
    error.setAttribute("role", "alert");
    const answer = (t) => t.trim() && sendAnswer(m, t.trim(), error);
    input.addEventListener("keydown", (e) => { if (e.key === "Enter" && !e.isComposing) { e.preventDefault(); answer(input.value); } });
    const row = el("div", "row");
    for (const c of m.choices || []) row.append(btn(c, "pri", () => answer(c)));
    row.append(input, btn("Send reply", "", () => answer(input.value)), btn("Mark handled without replying", "link", () => dismissQuestion(m, error)));
    const card = el("div", "qcard", el("div", "sub", m.from + " in " + roomLabel(m.room)), el("div", "txt", m.text || ""), row, error);
    card.dataset.key = m.key;
    card.dataset.version = m.version;
    if (prev) prev.replaceWith(card); else box.append(card);
  }
  for (const obsolete of existing.values()) obsolete.remove();
}
// NOT_CAUGHT_UP is shown before the second approval (node.NotCaughtUpWarning).
const NOT_CAUGHT_UP = "This machine has not checked the room's log with another member since it started. " +
  "If it was restored from a backup, approving now can damage the room's admin record (two different entries " +
  "signed at one position, a fork). Approve again only if every other member is gone or you are sure this " +
  "machine holds the room's latest log.";
async function decide(r, approve, row, notCaughtUpOK) {
  for (const b of row.querySelectorAll("button")) b.disabled = true;
  try {
    const o = await api("/api/confirm", { id: r.id, approve, text: r.text, not_caught_up_ok: !!notCaughtUpOK });
    if (o.reason === "not_caught_up") {
      S.risky.add(r.id);
      S.outcome = "Request " + fp(r.id) + " is not decided: this machine has not caught up with the room. Read the warning below.";
    } else {
      S.risky.delete(r.id);
      S.outcome = "Request " + fp(r.id) + ": " + o.state + (o.reason ? " (" + o.reason + ")" : "");
    }
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
async function sendAnswer(m, text, error) {
  let sent = false;
  try {
    if (!questionOnScreen(m)) throw new Error("the question changed; read the new version first");
    await api("/api/send", { room: m.room, text, reply_to: m.id });
    sent = true;
    await finishQuestion(m);
  } catch (e) {
    showQuestionError(m, error, (sent ? "Reply sent. Could not mark the question handled. " : "Could not send the reply. ") + e.message);
  }
}
async function dismissQuestion(m, error) {
  try { await finishQuestion(m); } catch (e) { showQuestionError(m, error, e.message); }
}
function showQuestionError(m, error, message) {
  // A partial success may remove the card on the next poll.
  if (error.isConnected && S.questions.some((q) => q.key === m.key && q.version === m.version)) error.textContent = message;
  else S.outcome = message;
  refresh(true);
}

// ---- stream ----
function entryMeta(m) {
  const meta = el("div", "meta", el("span", "from", m.from), el("span", "", fmt(m.created)));
  if (m.to && m.to.length) meta.append(el("span", "", "to " + m.to.join(", ")));
  if (m.parent) meta.append(el("span", "", "reply to " + fp(m.parent) + (m.parent_state && m.parent_state !== "here" ? " (" + m.parent_state + ")" : "")));
  if (m.author_removed) meta.append(el("span", "mark", "removed machine"));
  if (m.late) meta.append(el("span", "mark", "late"));
  if (m.ask_handled) meta.append(el("span", "mark", "asks for handled reports"));
  return meta;
}
// ---- files on a message ----
const FILE_STATES = {
  here: "on this machine", not_fetched: "not fetched yet", partial: "downloading",
  coming: "asked of its author", unavailable: "unavailable: no member served it",
};
const FETCH_STATES = {
  partial: "still downloading; try again shortly", coming: "asked of its author, who sends it when it next connects",
  fetching: "downloading; try again shortly", unavailable: "unavailable: no member served it",
  gone: "gone", too_large: "too large for this machine's file budget", changed: "this machine's copy changed; it is not served",
};
// fileNotes: what the last download of a file answered, keyed message and
// sha256, so a redraw keeps it.
const fileNotes = new Map();
// redraw draws the view the files are in again (the stream by default).
function filesOf(m, redraw) {
  if (!(m.files || []).length && !m.files_pending && !m.files_note) return null;
  const box = el("ul", "files");
  box.setAttribute("aria-label", "Files");
  for (const f of m.files || []) {
    const key = m.id + "/" + f.sha256;
    const li = el("li", "", el("span", "fname", escapeDisplay(f.name)), " ",
      el("span", "sub", sizeText(f.size) + ", " + (FILE_STATES[f.state] || f.state)));
    if (f.state === "here") li.append(" ", btn("Download", "link", () => download(m, f, redraw), "Save " + escapeDisplay(f.name) + " to this computer"));
    else if (f.state === "not_fetched" || f.state === "unavailable") li.append(" ", btn("Fetch", "link", () => download(m, f, redraw), "Ask the room's members for this file"));
    if (fileNotes.has(key)) li.append(el("div", "sub", fileNotes.get(key)));
    box.append(li);
  }
  if (m.files_pending) box.append(el("li", "sub", "The list of files is on its way."));
  if (m.files_note) box.append(el("li", "sub", "Files: " + m.files_note));
  return box;
}
// download asks for one file as the person. The page cannot send its token
// with a plain link, so it fetches the bytes with the token and saves them
// through a blob URL and the download attribute; nothing is opened.
async function download(m, f, redraw) {
  const again = redraw || (() => renderStream(true));
  const key = m.id + "/" + f.sha256;
  fileNotes.set(key, "Getting the file...");
  again();
  try {
    const q = "room=" + encodeURIComponent(m.room) + "&msg=" + encodeURIComponent(m.id) + "&sha=" + encodeURIComponent(f.sha256);
    const r = await fetch("/api/file?" + q, { headers: { Authorization: "Bearer " + token } });
    if (r.status === 401) { showAuthFail(); return; }
    const ct = r.headers.get("Content-Type") || "";
    if (!r.ok || !ct.startsWith("application/octet-stream")) {
      let j = null;
      try { j = await r.json(); } catch (e) { /* non-JSON */ }
      const state = j && j.state;
      fileNotes.set(key, state ? "Not saved: " + (FETCH_STATES[state] || state) + (j.reason ? " (" + j.reason + ")" : "")
        : "Not saved: " + ((j && j.error) || "request failed (" + r.status + ")"));
    } else {
      const url = URL.createObjectURL(await r.blob());
      const a = el("a");
      a.href = url;
      a.download = escapeDisplay(f.name).replace(/["'\/\\:*?<>|]/g, "_").replace(/^[. ]+|[. ]+$/g, "") || "file";
      a.hidden = true;
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 60000);
      fileNotes.set(key, "Saved by the browser as a download.");
    }
  } catch (e) { fileNotes.set(key, "Not saved: " + e.message); }
  again();
  refresh(true);
}

// Notices stay in S.notes until the room is left, so they remain visible
// after being confirmed.
function renderStream(force) {
  const list = all().filter((m) => m.text);
  const notes = S.notes;
  const sig = S.room + "|" + list.map((m) => m.id + "@" + m.version + (m.files || []).map((f) => ":" + f.state).join("") +
    (m.files_pending ? ":p" : "") + (m.files_note || "")).join() + "|" + notes.map((m) => m.key + "@" + m.version).join() + "|" + S.more;
  if (sig === S.sigs.s && !force) return;
  S.sigs.s = sig;
  S.onscreen = new Map(list.map((m) => [m.key, m.version]));
  const sc = $("stream");
  const atBottom = sc.scrollHeight - sc.scrollTop - sc.clientHeight < 40;
  const box = $("msgs");
  box.replaceChildren();
  for (const m of list) {
    const meta = entryMeta(m);
    meta.append(el("span", "actions", btn("Reply", "link", () => setReply(m)), btn("Thread", "link", () => openThread(m.thread || m.id)),
      btn("Trace", "link", () => openTrace(m.id))));
    const d = el("div", "msg" + (m.from === S.st.human ? " mine" : ""), meta, el("div", "body", m.text), filesOf(m));
    box.append(d);
  }
  // A notice is drawn with what it says: a replacement with its new text,
  // so that confirming it confirms text the person saw.
  S.notesShown = new Map();
  for (const n of notes) {
    const what = { replaced: " changed message ", withdrawn: " withdrew message ", expired: ": the text expired of message " }[n.kind] || " " + n.kind + " ";
    const line = el("div", "noteline", (n.from || "") + what + fp(n.id) + " (" + fmt(n.created) + ")");
    if (n.kind === "replaced" && n.text) line.append(el("div", "body", n.text));
    box.append(line);
    S.notesShown.set(n.key, n.version);
  }
  $("older").hidden = !S.more || !S.msgs.length;
  $("empty").hidden = !S.room || list.length > 0 || notes.length > 0 || all().length > 0;
  $("install-tip").hidden = (S.st.agents || []).some((a) => a.id !== S.st.human);
  if (atBottom) sc.scrollTop = sc.scrollHeight;
}
function updateCompose() {
  const r = room(), ready = !!r && S.shown === S.room;
  $("text").placeholder = r ? "Message " + r.label : "Select a room";
  const b = $("compose").querySelector("button[type=submit]");
  b.disabled = !ready;
}
// node.MaybeRemovedText
const REMOVED = "this machine may have been removed from this room; ask an admin";
function renderHead() {
  const r = room();
  $("roomhead").textContent = r ? roomLabel(r.id) + (S.waiting ? " (fetching " + S.waiting + " more)" : "") : "";
  const note = $("roomnote");
  note.hidden = !(r && r.may_be_removed);
  if (note.textContent !== REMOVED) note.textContent = REMOVED;
  updateCompose();
  if (S.toRoom !== S.room) { S.toRoom = S.room; $("to").value = ""; }
  const senders = new Set();
  for (const m of all()) if (m.from !== S.st.human) senders.add(m.from);
  const sig = JSON.stringify([S.room, S.reply && S.reply.from, [...senders]]);
  if (sig === S.toSig) return;
  S.toSig = sig;
  const sel = $("to"), cur = sel.value;
  sel.replaceChildren(el("option", "", S.reply ? S.reply.from + " (reply)" : "Everyone"));
  sel.firstChild.value = "";
  for (const s of senders) { const o = el("option", "", s); o.value = s; sel.append(o); }
  sel.value = cur;
}
function setReply(m) {
  S.reply = m;
  renderHead();
  const r = $("replying");
  r.hidden = false;
  r.replaceChildren("Replying to " + m.from + ": " + (m.text || "").slice(0, 60) + " ", btn("Cancel reply", "link", () => { S.reply = null; r.hidden = true; renderHead(); $("text").focus(); }));
  $("text").focus();
}

// ---- dialogs ----
function showDialog(...kids) {
  kids[0].id = "dialog-title";
  $("tracebody").replaceChildren(...kids);
  if (!$("trace").open) $("trace").showModal();
}
async function openTrace(id) {
  try {
    const t = await api("/api/trace?id=" + encodeURIComponent(id));
    const ct = el("table"), ht = el("table");
    ct.append(el("tr", "", ...["Machine", "Observed", "Member now"].map((h) => el("th", "", h))));
    for (const c of t.copies || []) ct.append(el("tr", "", el("td", "", fp(c.node)), el("td", "", fmt(c.at)), el("td", "", c.member ? "yes" : "no")));
    ht.append(el("tr", "", ...["Agent", "When"].map((h) => el("th", "", h))));
    for (const h of t.handled || []) ht.append(el("tr", "", el("td", "", h.agent), el("td", "", fmt(h.at))));
    // The same words as a2p2 trace (cmd/a2p2, textWords and copyReportWords).
    const textState = {
      live: "available on this machine", missing: "waiting for a copy",
      not_admitted: "outside this machine's permitted room history",
      withdrawn: "withdrawn by its author", expired: "expired (text is kept 30 days)",
    }[t.reason] || t.reason;
    showDialog(el("h2", "", "Message " + fp(id)),
      el("p", "", "Header: " + (t.saved ? "saved on this machine" : "not held on this machine") + ". Text: " + textState + "."),
      el("h2", "", "Copy reports"),
      el("p", "sub", "These are the machines that reported holding the author's messages up to this one, as this machine observed."),
      (t.copies || []).length ? ct : el("div", "sub", "None yet."),
      el("h2", "", "Handled"), (t.handled || []).length ? ht : el("div", "sub", "No handling reports yet."));
  } catch (e) { showDialog(el("h2", "", "Could not open message trace"), el("p", "", e.message)); }
}
async function openThread(id) {
  try {
    const t = await api("/api/thread?id=" + encodeURIComponent(id));
    const kids = [el("h2", "", "Thread " + fp(id))];
    for (const m of t.entries || []) kids.push(el("div", "thr", entryMeta(m), el("div", "body", m.text || m.kind), filesOf(m, () => openThread(id))));
    if (kids.length === 1) kids.push(el("div", "sub", "Nothing in this thread."));
    showDialog(...kids);
  } catch (e) { showDialog(el("h2", "", "Could not open thread"), el("p", "", e.message)); }
}

// ---- data flow ----
const loadHistory = (rid, extra, limit) => api("/api/history?room=" + encodeURIComponent(rid) + "&limit=" + (limit || 100) + (extra || ""));
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
  const load = id !== S.room || S.shown !== id;
  if (load) switchTo(id);
  if (matchMedia("(max-width: 64rem)").matches) $("roomhead").focus();
  if (!load) return;
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
      const page = h.entries || [];
      // Cached older pages are fetched again from the newest page's
      // boundary, so text replaced, withdrawn or expired since leaves them.
      let older = [], more = page.length >= 100;
      if (more && S.older.length) {
        const want = Math.min(S.older.length, 400);
        const o = await loadHistory(rid, "&before=" + encodeURIComponent(page[0].id), want);
        if (gen !== S.gen) { again = true; return; }
        older = o.entries || [];
        more = older.length < want ? false : S.more;
      }
      S.more = more;
      S.msgs = page;
      S.older = older;
      S.boundary = page.length ? page[0].id : "";
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
        : S.notesShown.get(m.key) === m.version);
      if (mine.length) {
        await confirmSeen(mine);
        if (gen !== S.gen) { again = true; return; }
      }
    }
    renderNotice(); renderSide(); renderRoomFiles(); renderNeeds(); renderHead(); renderStream(force);
    $("connection").hidden = true;
  } catch (e) {
    if (e.message !== "unauthorized") {
      const status = $("connection");
      const message = "Could not load the page data. Retrying. " + e.message;
      if (status.textContent !== message) status.textContent = message;
      status.hidden = false;
    }
  }
  finally {
    busy = false;
    if (again) { again = false; refresh(true); }
  }
}
$("older").addEventListener("click", async () => {
  const first = all().find((m) => m.id);
  if (!first || S.shown !== S.room) return;
  const gen = S.gen, rid = S.room, boundary = S.boundary;
  try {
    const page = ((await loadHistory(rid, "&before=" + encodeURIComponent(first.id))).entries) || [];
    // Dropped if the room or the newest page's boundary changed meanwhile.
    if (gen !== S.gen || boundary !== S.boundary || first !== all().find((m) => m.id)) return;
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

const desktop = matchMedia("(min-width: 64.001rem)");
function setDetailsLayout() { document.querySelector(".machine-details").open = desktop.matches; }
setDetailsLayout();
desktop.addEventListener("change", setDetailsLayout);
if (!token) showAuthFail();
else { refresh(true); setInterval(refresh, 2000); }
