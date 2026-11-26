"use strict";
// All peer-supplied text is rendered with textContent only; never markup.
const $ = (id) => document.getElementById(id);
// notice: the one-time upgrade notice, kept for the life of the page because
// the server returns it only once. onscreen: key -> version of every entry as
// rendered; only that is ever confirmed. qShown: the same for question cards.
const S = { st: null, unread: [], questions: [], qKeys: new Set(), waiting: 0, alerts: [], requests: [], room: null, msgs: [], older: [], notes: [], more: true, reply: null, sigs: {}, toSig: "", gen: 0, shown: null, toRoom: null, outcome: "", notice: "", risky: new Set(), onscreen: new Map(), qShown: new Map(), notesShown: new Map(), boundary: "", toChosen: "" };
// Sign-in (PROTOCOL 7). The address a2p2 ui opens carries a one-time code
// (#c=...), which works once and for 30 seconds; the page trades it for a
// session, kept in this tab only. run is a random value the daemon keeps
// with that session alone and puts in each state the session reads: a
// state with any other run signs the page out, since after the daemon
// stops another local user may answer on its port.
// verified is true only while the last state named that run; any
// failed or foreign state clears it. A call other than the state is made
// only while verified, and a POST or a download first asks for a fresh
// state and goes out only if that state names the run, so what the person
// types goes to another listener only if it took the port in the moment
// between that check and the call. signedOut stops every call.
let token = "", run = "", code = "", signedOut = "", verified = false;
const NOT_CONNECTED = "Not connected to this machine's a2p2 daemon, so nothing was sent. The page retries on its own.";
try { token = sessionStorage.getItem("a2p2.session") || ""; run = sessionStorage.getItem("a2p2.run") || ""; } catch (e) { /* storage blocked */ }
if (location.hash.length > 1) {
  const h = decodeURIComponent(location.hash.slice(1));
  if (h.startsWith("c=")) code = h.slice(2);
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
// ---- display escaping (escape:begin) ----
// Text from other machines must not change what the person reads. These
// are node.EscapeDisplay, EscapeDisplayText, EscapeChoice, DisplayHidden
// and ChoiceHidden (internal/node/escape.go, PROTOCOL 10.8), with the same
// tables and the same rules; Go tests keep the two equal, on the strings
// of internal/node/testdata/escape_cases.json among others. Each
// character of HIDDEN is written \u{hex} unless the text rules keep it
// where it stands, and a backslash before "u{" or "x{" is written
// \u{5c}; nothing is dropped.
const HIDDEN = [
  [0x0000, 0x001f], [0x007f, 0x009f], [0x00ad, 0x00ad], [0x034f, 0x034f], [0x0600, 0x0605],
  [0x061c, 0x061c], [0x06dd, 0x06dd], [0x070f, 0x070f], [0x0890, 0x0891], [0x08e2, 0x08e2],
  [0x115f, 0x1160], [0x17b4, 0x17b5], [0x180b, 0x180f], [0x200b, 0x200f], [0x2028, 0x202e],
  [0x2060, 0x206f], [0x3164, 0x3164], [0xd800, 0xdfff], [0xfe00, 0xfe0f], [0xfeff, 0xfeff],
  [0xffa0, 0xffa0], [0xfff0, 0xfffb], [0x110bd, 0x110bd], [0x110cd, 0x110cd], [0x13430, 0x1343f],
  [0x1bca0, 0x1bca3], [0x1d173, 0x1d17a], [0xe0000, 0xe0fff],
];
// EMOJI: a joiner may stand between two, a selector after one.
const EMOJI = [
  [0x00a9, 0x00a9], [0x00ae, 0x00ae], [0x203c, 0x203c], [0x2049, 0x2049],
  [0x2122, 0x2122], [0x2139, 0x2139], [0x2194, 0x21aa], [0x231a, 0x23ff],
  [0x24c2, 0x24c2], [0x25aa, 0x27bf], [0x2934, 0x2935], [0x2b05, 0x2b55],
  [0x3030, 0x3030], [0x303d, 0x303d], [0x3297, 0x3297], [0x3299, 0x3299],
  [0x1f000, 0x1faff],
];
// JOINING: scripts whose letters take a joiner or non-joiner.
const JOINING = [
  [0x0600, 0x08ff], [0x0900, 0x0dff], [0x1000, 0x109f], [0x1780, 0x18af],
  [0xfb50, 0xfdff], [0xfe70, 0xfefe],
];
// RTL_LETTERS: letters of right-to-left scripts; a right-to-left or Arabic
// letter mark may stand between two of them. Code points, not a Unicode
// class, so that every browser gives the daemon's answer.
const RTL_LETTERS = [
  [0x05d0, 0x05ea], [0x05ef, 0x05f2], [0x0620, 0x064a], [0x066e, 0x066f], [0x0671, 0x06d3],
  [0x06d5, 0x06d5], [0x06ee, 0x06ef], [0x06fa, 0x06fc], [0x06ff, 0x06ff], [0x0710, 0x0710],
  [0x0712, 0x072f], [0x074d, 0x07a5], [0x07b1, 0x07b1], [0x07ca, 0x07ea], [0x0800, 0x0815],
  [0x0840, 0x0858], [0x0860, 0x086a], [0x0870, 0x0887], [0x0889, 0x088e], [0x08a0, 0x08c9],
  [0xfb1d, 0xfb1d], [0xfb1f, 0xfb28], [0xfb2a, 0xfb36], [0xfb38, 0xfb3c], [0xfb3e, 0xfb3e],
  [0xfb40, 0xfb41], [0xfb43, 0xfb44], [0xfb46, 0xfbb1], [0xfbd3, 0xfd3d], [0xfd50, 0xfd8f],
  [0xfd92, 0xfdc7], [0xfdf0, 0xfdfb], [0xfe70, 0xfe74], [0xfe76, 0xfefc],
];
// SPACELESS: scripts written without spaces; a zero width space may stand
// between two of their characters.
const SPACELESS = [
  [0x0e00, 0x0fff], [0x1000, 0x109f], [0x1780, 0x17ff],
];
// HAN: CJK ideographs; one ideographic variation selector may follow one.
const HAN = [
  [0x3400, 0x4dbf], [0x4e00, 0x9fff], [0xf900, 0xfaff], [0x20000, 0x3ffff],
];
// FLAGS: the tag sequences after U+1F3F4 of the flags of England,
// Scotland and Wales, with the cancel tag.
const FLAGS = ["gbeng", "gbsct", "gbwls"].map((s) => Array.from(s, (ch) => 0xe0000 + ch.codePointAt(0)).concat([0xe007f]));
function inRanges(tab, c) {
  let lo = 0, hi = tab.length - 1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1, r = tab[mid];
    if (c < r[0]) hi = mid - 1; else if (c > r[1]) lo = mid + 1; else return true;
  }
  return false;
}
const hidden = (c) => inRanges(HIDDEN, c);
const esc = (c) => "\\u{" + c.toString(16) + "}";
const str = (s) => String(s == null ? "" : s);
// textClass is what a character is to the text rules, as bits as in Go.
const EMOJI_C = 1, SEL_C = 2, JOIN_C = 4, KEYCAP_C = 8, RTL_C = 16, SPACELESS_C = 32, HAN_C = 64;
function textClass(c) {
  if (c === undefined || hidden(c)) return 0;
  let k = 0;
  if ((c >= 0x30 && c <= 0x39) || c === 0x23 || c === 0x2a) k |= KEYCAP_C;
  if (inRanges(EMOJI, c)) k |= EMOJI_C;
  if (inRanges(JOINING, c)) k |= JOIN_C;
  if (inRanges(RTL_LETTERS, c)) k |= RTL_C;
  if (inRanges(SPACELESS, c)) k |= SPACELESS_C;
  if (inRanges(HAN, c)) k |= HAN_C;
  return k;
}
// flagTail is the length of the flag tag sequence at cs[i], or 0.
function flagTail(cs, i) {
  for (const t of FLAGS) if (t.every((c, k) => cs[i + k] === c)) return t.length;
  return 0;
}
// escapeRules is node.escapeText: the text rules, with newlines and tabs
// kept (and a CR before a newline shown as that line break) when lines is
// true. literal false skips the backslash rule, for text the daemon has
// escaped already, so that escaping it again changes nothing.
function escapeRules(s, lines, literal = true) {
  const cs = Array.from(str(s), (ch) => ch.codePointAt(0));
  let out = "", prev = 0;
  for (let i = 0; i < cs.length; i++) {
    const c = cs[i];
    const tail = c === 0x1f3f4 ? flagTail(cs, i + 1) : 0;
    if (tail) {
      out += String.fromCodePoint(...cs.slice(i, i + 1 + tail));
      i += tail; prev = EMOJI_C;
      continue;
    }
    if (lines && c === 0x0d && cs[i + 1] === 0x0a) { out += "\n"; i++; prev = 0; continue; }
    const nc = textClass(cs[i + 1]);
    let hide = hidden(c), next = 0;
    if (c === 0x0a || c === 0x09) hide = !lines;
    else if (c === 0x5c && literal && (cs[i + 1] === 0x75 || cs[i + 1] === 0x78) && cs[i + 2] === 0x7b) hide = true;
    else if ((c === 0x200d && (prev & (EMOJI_C | SEL_C)) && (nc & EMOJI_C)) ||
      ((c === 0x200c || c === 0x200d) && (prev & JOIN_C) && (nc & JOIN_C)) ||
      ((c === 0x200f || c === 0x061c) && (prev & RTL_C) && (nc & RTL_C)) ||
      (c === 0x200b && (prev & SPACELESS_C) && (nc & SPACELESS_C)) ||
      (c >= 0xe0100 && c <= 0xe01ef && (prev & HAN_C))) hide = false;
    else if ((c === 0xfe0e || c === 0xfe0f) && (prev & EMOJI_C)) { hide = false; next = SEL_C; }
    else if ((c === 0xfe0e || c === 0xfe0f) && (prev & KEYCAP_C)) hide = false;
    else if (!hide) next = textClass(c);
    out += hide ? esc(c) : String.fromCodePoint(c);
    prev = next;
  }
  return out;
}
// escapeDisplay is a string shown as one line: line breaks and tabs too.
const escapeDisplay = (s) => escapeRules(s, false);
// displayHidden: escapeDisplay would change s.
const displayHidden = (s) => escapeDisplay(s) !== str(s);
// escapeText is message text: lines stay lines.
const escapeText = (s) => escapeRules(s, true);
// blankChoice: a space other than U+0020, or blank braille.
const blankChoice = (c) => c === 0x2800 || c === 0xa0 || c === 0x1680 || (c >= 0x2000 && c <= 0x200a) ||
  c === 0x202f || c === 0x205f || c === 0x3000;
// escapeChoice is a choice as its button shows it: one line, and every
// blank that would not be sent as shown (a space other than U+0020, blank
// braille, a space at either end or next to another) written \u{...}.
function escapeChoice(s) {
  const cs = Array.from(escapeRules(s, false), (ch) => ch.codePointAt(0));
  let out = "";
  for (let i = 0; i < cs.length; i++) {
    const c = cs[i];
    if (blankChoice(c)) out += esc(c);
    else if (c === 0x20 && (i === 0 || i === cs.length - 1 || cs[i - 1] === 0x20 || cs[i + 1] === 0x20)) out += esc(c);
    else out += String.fromCodePoint(c);
  }
  return out;
}
// choiceHidden: the button would not show s exactly as it is sent.
const choiceHidden = (s) => escapeChoice(s) !== str(s);
// iso sets a remote string apart in a sentence (a bdi element), so that
// right-to-left text in it cannot reorder the words around it.
const iso = (s) => el("bdi", "", s);
// isolated draws a sentence the daemon wrote with node.Isolate: each part
// between U+2068 and U+2069 in a bdi element, and every part escaped
// again without the backslash rule (the daemon has escaped it).
function isolated(text) {
  const out = [];
  let rest = str(text);
  while (rest) {
    const open = rest.indexOf("\u2068");
    const before = open < 0 ? rest : rest.slice(0, open);
    if (before) out.push(escapeRules(before, false, false));
    if (open < 0) break;
    rest = rest.slice(open + 1);
    const close = rest.indexOf("\u2069");
    out.push(iso(escapeRules(close < 0 ? rest : rest.slice(0, close), false, false)));
    rest = close < 0 ? "" : rest.slice(close + 1);
  }
  return out;
}
// ---- escape:end ----
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
  if (signedOut) throw new Error(signedOut);
  const isState = path === "/api/state";
  if (!isState) await checkRun(body !== undefined);
  const opt = { headers: { Authorization: "Bearer " + token } };
  if (body !== undefined) {
    opt.method = "POST";
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  try {
    const r = await fetch(path, opt);
    let j = null;
    try { j = await r.json(); } catch (e) { /* non-JSON */ }
    if (r.status === 401) { showAuthFail(); throw new Error("unauthorized"); }
    if (!r.ok) throw new Error((j && j.error) || "request failed (" + r.status + ")");
    if (isState && (!j || j.run !== run)) {
      showAuthFail("This page belongs to an earlier run of the a2p2 daemon, or something else answers on its port.");
      throw new Error(signedOut);
    }
    if (isState) verified = true;
    return j;
  } catch (e) {
    if (isState) verified = false; // any failed state: unverified until one names the run again
    throw e;
  }
}
// checkRun throws unless the last state came from this page's daemon
// process. For a POST or a download (post true) it first asks for a fresh
// state. Nothing is sent when it throws.
async function checkRun(post) {
  if (post) {
    try { await api("/api/state"); } catch (e) { /* verified is false now */ }
  }
  if (signedOut) throw new Error(signedOut);
  if (!verified) throw new Error(NOT_CONNECTED);
}
// showAuthFail signs the page out: no further call is made.
function showAuthFail(why) {
  signedOut = "signed out";
  if (why) $("authwhy").textContent = why;
  $("connection").hidden = true; $("app").hidden = true; $("authfail").hidden = false;
}
// trade swaps the link's code for a session.
async function trade() {
  const r = await fetch("/api/session", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ code }) });
  let j = null;
  try { j = await r.json(); } catch (e) { /* non-JSON */ }
  if (!r.ok || !j || !j.session) { token = ""; showAuthFail((j && j.error) || "This link did not work (" + r.status + ")."); return; }
  token = j.session; run = j.run || "";
  try { sessionStorage.setItem("a2p2.session", token); sessionStorage.setItem("a2p2.run", run); } catch (e) { /* ignore */ }
}

const rooms = () => (S.st && S.st.rooms) || [];
const room = () => rooms().find((r) => r.id === S.room) || null;
// roomLabel is how the page names a room: its label, which the room's
// creator chose and may be on another machine, escaped; with its id when
// another room's label could be drawn the same. Labels are compared as
// drawn: escaped, in Unicode normal form C (so "café" composed and
// decomposed match), without the joiners, marks and selectors the text
// rules keep, with each run of blank characters as one space and
// trimmed, and without case. Blank is every Unicode white space character
// (a no-break space, an ideographic space and the other spaces among
// them) and U+2800, blank braille, as in node.LabelKey. So "Ops" and
// "ops", "a  b" and "a b", or "a\u3000b" and "a b" get their ids.
const KEPT_INVISIBLE = /[\u200b\u200c\u200d\u200f\u061c\ufe0e\ufe0f\u{e0100}-\u{e01ef}]/gu;
const LABEL_BLANK = /[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u2800\u3000]+/gu;
const drawnLabel = (l) => escapeDisplay(l).normalize("NFC").replace(KEPT_INVISIBLE, "")
  .replace(LABEL_BLANK, " ").replace(/^ | $/g, "").toLowerCase();
function roomParts(id) {
  const entries = rooms();
  const r = entries.find((r) => r.id === id);
  if (!r) return [escapeDisplay(id), ""];
  const same = entries.some((other) => other.id !== id && drawnLabel(other.label) === drawnLabel(r.label));
  return [escapeDisplay(r.label), same ? " (" + escapeDisplay(id) + ")" : ""];
}
// roomLabel is that name as one string, for an attribute; isolate
// characters set the label apart from the words around it.
const roomLabel = (id) => { const [l, idText] = roomParts(id); return "\u2068" + l + "\u2069" + idText; };
// roomNode is that name drawn in a sentence: the label in a bdi element.
const roomNode = (id) => { const [l, idText] = roomParts(id); return [iso(l), idText]; };
// machineName is how the page names a machine of a room: the name that
// room records for it, escaped, with its short id; never a name another
// room gave it.
function machineName(node, roomId) {
  if (S.st && node === S.st.node) return "this machine";
  const r = rooms().find((r) => r.id === roomId);
  const m = ((r && r.members) || []).find((m) => m.node === node);
  return m && m.name ? escapeDisplay(m.name) + " " + fp(node) : "machine " + fp(node);
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
  const choices = rooms().map((r) => [r.id, roomParts(r.id), unreadFor(r.id).length,
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
      b.append(el("span", "l", iso(label[0]), label[1], el("span", "sub" + (removed ? " warn" : ""),
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
    box.append(el("h2", "", "Members of ", ...roomNode(r.id)));
    for (const m of r.members || []) {
      const adm = m.admin || (r.admins || []).includes(m.node);
      // A machine with no name (a room's creator, seen from elsewhere) shows its short id once.
      const shared = (r.members || []).length > 1;
      box.append(el("div", "mem", m.name ? escapeDisplay(m.name) : fp(m.node), m.name ? " " : null, m.name ? el("span", "muted", fp(m.node)) : null,
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
    box.append(el("div", "", p.name ? escapeDisplay(p.name) : fp(p.node), p.name ? " " : null, p.name ? el("span", "muted", fp(p.node)) : null),
      el("div", "sub", p.last_heard ? "heard " + ago(p.last_heard) : "not heard from yet"));
  }
  box.append(el("h2", "", "Local agents"));
  // Each agent's reading level per room (U1): only levels other than "all"
  // are listed. The person reads every room in full here, whatever any
  // agent's level.
  const levelRoom = (id) => escapeDisplay(((S.st.rooms || []).find((r) => r.id === id) || {}).label || id);
  for (const a of S.st.agents || []) {
    box.append(el("div", "", a.label || a.id, " ", el("span", "sub", a.harness + " · " + ago(a.last_seen))));
    if (a.label === "human") continue; // the person: its id may be written in full in S.st.human
    const ls = (S.st.levels || []).filter((l) => l.agent === a.id);
    const words = ls.map((l) => levelRoom(l.room) + ": " + l.level);
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
  box.append(el("h2", "", "Files in ", ...roomNode(r.id)),
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
  // The daemon escaped every remote string in the text; escaping it again
  // changes nothing and keeps a page served by an older daemon safe.
  const c = el("div", "qcard", el("div", "sub", "Request " + fp(r.id)), el("div", "txt", ...isolated(r.text)));
  const due = r.deadline && !String(r.deadline).startsWith("0001") ? (fmt(r.deadline) || r.deadline) : "";
  if (r.kind === "send_files") {
    c.append(sendTerms(r));
    if (due) c.append(el("div", "sub", "Expires " + due + ", declined unless approved"));
  } else {
    if (r.name && r.kind !== "network") c.append(el("div", "sub", NAME_NOTES[r.name_from] || NAME_NOTES[""]));
    if (due) c.append(el("div", "sub", "Invite expires " + due));
  }
  c.append(row);
  return c;
}
// NAME_NOTES say who chose a request's name (node.NameFrom*), in the
// words of a2p2 confirm (cmd/a2p2 nameNote).
const NAME_NOTES = {
  machine: "The machine chose its own name. Check its fingerprint.",
  admin: "The admin who signed the first entry chose this name. Check the fingerprint against the room's members.",
  here: "The name was given on this machine. Check the fingerprint.",
  "": "Check the machine's fingerprint, not its name.",
};
// sendTerms shows every term of a send with files, as a2p2 confirm does
// (cmd/a2p2 printSendTerms). The daemon has escaped controls and
// bidirectional characters in paths, names and lines; the room's label is
// escaped here the same way.
function sendTerms(r) {
  const t = el("dl", "terms");
  const row = (k, ...v) => t.append(el("dt", "", k), el("dd", "", ...v));
  row("agent", r.agent || "");
  row("room", iso(escapeDisplay(r.room_label)), " (" + r.room + ")");
  const to = r.to || [];
  if (to.length === 1 && to[0] === "everyone") row("to", "everyone in the room (" + (r.members || 0) + " members)");
  else if (to.length) row("to", to.join(", "));
  else if (r.reply_to) row("to", (r.reply_author ? r.reply_author + ", " : "") + "the author of message " + r.reply_to);
  if (r.reply_to) row("reply to", r.reply_to);
  if (r.replaces) row("replaces", r.replaces.id + ": \"" + r.replaces.line + "\"",
    ...(r.replaces.version ? [el("div", "sub", "its version " + r.replaces.version)] : []),
    el("div", "sub", "the replacement keeps its recipients and has only the files listed here"));
  // the whole text, each line escaped by the daemon; shown as text, never markup
  row("text", el("pre", "body", r.body || r.text_line || ""));
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
  for (const a of alerts) al.append(el("div", "warnline", a.kind + " in ", ...roomNode(a.room), ": " + escapeDisplay(a.text) + (a.since ? " (since " + fmt(a.since) + ")" : "")));
  $("questions-title").hidden = !qs.length;
  const box = $("questions");
  // Keep a draft and its focused control when unrelated alerts or mail arrive.
  const existing = new Map(Array.from(box.children, (card) => [card.dataset.key, card]));
  S.qShown = new Map();
  for (const m of qs) {
    S.qShown.set(m.key, m.version);
    const prev = existing.get(m.key);
    existing.delete(m.key);
    if (prev && prev.dataset.version === m.version) {
      prev.firstChild.replaceChildren(...askedBy(m));
      continue;
    }
    const input = el("input");
    input.placeholder = "Write a reply";
    input.setAttribute("aria-label", "Reply to \u2068" + escapeDisplay(m.from) + "\u2069 in " + roomLabel(m.room));
    const error = el("div", "question-error");
    error.setAttribute("role", "alert");
    const answer = (t) => t.trim() && sendAnswer(m, t.trim(), error);
    input.addEventListener("keydown", (e) => { if (e.key === "Enter" && !e.isComposing) { e.preventDefault(); answer(input.value); } });
    const row = el("div", "row");
    // A choice is the asker's text, sent as the person's own reply: it is
    // drawn escaped, in the plain style (never as an approval button), and
    // one holding characters that do not show is not sent; the person
    // types the reply instead.
    for (const c of m.choices || []) {
      row.append(btn(escapeChoice(c), "choice", () => choiceHidden(c)
        ? showQuestionError(m, error, HIDDEN_CHOICE) : answer(c)));
    }
    row.append(input, btn("Send reply", "", () => answer(input.value)), btn("Mark handled without replying", "link", () => dismissQuestion(m, error)));
    const card = el("div", "qcard ask", el("div", "sub", ...askedBy(m)), el("div", "sub", ASK_NOTE),
      el("div", "txt", escapeText(m.text || "")), row, error);
    card.dataset.key = m.key;
    card.dataset.version = m.version;
    if (prev) prev.replaceWith(card); else box.append(card);
  }
  for (const obsolete of existing.values()) obsolete.remove();
}
// askedBy names who asked a question and of whom: the agent, its machine
// and the room, and whether it was asked of everyone in the room.
// A message addressed only to other agents is not the person's to answer
// and is never among the questions (node.Surface.Questions).
function askedBy(m) {
  const whom = (m.to || []).includes(S.st.human) ? ", asked of you" : ", asked of everyone in the room";
  const marks = authorMarks(m);
  return [iso(escapeDisplay(m.from)), marks.length ? " (" + marks.join(", ") + ")" : "", " on ",
    iso(machineName(m.from_node, m.room)), " in ", ...roomNode(m.room), whom];
}
const ASK_NOTE = "A message, not a request: a choice sends that text as your reply, and approves nothing.";
const HIDDEN_CHOICE = "This choice holds characters that do not show, that change how text reads, or blanks that " +
  "would not be sent as shown (written \\u{...}). " +
  "It was not sent. Type your reply instead.";
// NOT_CAUGHT_UP is shown before the second approval (node.NotCaughtUpWarning).
const NOT_CAUGHT_UP = "This machine has not checked the room's log with another member since it started. " +
  "If it was restored from a backup, approving now can damage the room's admin record (two different entries " +
  "signed at one position, a fork). Approve again only if every other member is gone or you are sure this " +
  "machine holds the room's latest log.";
async function decide(r, approve, row, notCaughtUpOK) {
  for (const b of row.querySelectorAll("button")) b.disabled = true;
  try {
    // terms: the digest the card showed; the daemon refuses an approval of a
    // send with files whose terms are not the request's current ones
    const o = await api("/api/confirm", { id: r.id, approve, text: r.text, terms: r.terms || "", not_caught_up_ok: !!notCaughtUpOK });
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
// authorMarks are the page's words for where a message's author is, from
// from_this_machine, which the daemon sets by the full machine key (D65),
// and whether its text is a replacement (edited, D62). Any member machine
// can post as human, so a human on another machine is never drawn as
// this machine's person.
function authorMarks(m) {
  const from = String(m.from || "");
  const label = from.split("@")[0];
  const person = label.toLowerCase() === "human";
  const out = [];
  if (from && person && !m.from_this_machine) out.push("another machine's person, not you");
  else if (from && person && label === "human") out.push("this machine's person");
  else if (from && person) out.push("this machine, not its person");
  else if (from && m.from_this_machine) out.push("this machine");
  if (m.id_shared) out.push("id written in full: short id shared");
  if (m.edited) out.push("edited");
  // changed_by is an agent of the author's machine (node.ChangedWords)
  if (m.changed_by) {
    const c = String(m.changed_by).split("@")[0];
    const who = c.toLowerCase() !== "human" ? "" : !m.from_this_machine ? ", another machine's person, not you"
      : c === "human" ? ", this machine's person" : ", this machine, not its person";
    out.push("changed by " + escapeDisplay(m.changed_by) + who);
  }
  return out;
}
// noteLine draws a notice (a replacement, a withdrawal, an expiry) with
// the same marks as the message, each in its own element, and a
// replacement's text in its own body element, so text can never stand
// where a mark is drawn.
function noteLine(n) {
  // from is the message's author; a mark names another agent of its machine that made the change
  const what = { replaced: ": new text of message ", withdrawn: ": withdrawn, message ", expired: ": the text expired of message " }[n.kind] || " " + n.kind + " ";
  const line = el("div", "noteline", el("span", "from", escapeDisplay(n.from || "")));
  for (const k of authorMarks(n)) line.append(" ", el("span", "mark", k));
  line.append(what + fp(n.id) + " (" + fmt(n.created) + ")");
  if (n.kind === "replaced" && n.text) line.append(el("div", "body", escapeText(n.text)));
  return line;
}
function entryMeta(m) {
  const meta = el("div", "meta", el("span", "from", escapeDisplay(m.from)), el("span", "", fmt(m.created)));
  for (const k of authorMarks(m)) meta.append(el("span", "mark", k));
  if (m.to && m.to.length) meta.append(el("span", "", "to " + m.to.map(escapeDisplay).join(", ")));
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
    await checkRun(true);
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
    const d = el("div", "msg" + (m.from === S.st.human && m.from_this_machine ? " mine" : ""), meta, el("div", "body", escapeText(m.text)), filesOf(m));
    box.append(d);
  }
  // A notice is drawn with what it says: a replacement with its new text,
  // so that confirming it confirms text the person saw.
  S.notesShown = new Map();
  for (const n of notes) {
    box.append(noteLine(n));
    S.notesShown.set(n.key, n.version);
  }
  $("older").hidden = !S.more || !S.msgs.length;
  $("empty").hidden = !S.room || list.length > 0 || notes.length > 0 || all().length > 0;
  $("install-tip").hidden = (S.st.agents || []).some((a) => a.label !== "human");
  if (atBottom) sc.scrollTop = sc.scrollHeight;
}
function updateCompose() {
  const r = room(), ready = !!r && S.shown === S.room;
  $("text").placeholder = r ? "Message " + roomLabel(r.id) : "Select a room";
  const b = $("compose").querySelector("button[type=submit]");
  b.disabled = !ready;
}
// node.MaybeRemovedText
const REMOVED = "this machine may have been removed from this room; ask an admin";
function renderHead() {
  const r = room();
  $("roomhead").replaceChildren(...(r ? [...roomNode(r.id), S.waiting ? " (fetching " + S.waiting + " more)" : ""] : []));
  const note = $("roomnote");
  note.hidden = !(r && r.may_be_removed);
  if (note.textContent !== REMOVED) note.textContent = REMOVED;
  updateCompose();
  if (S.toRoom !== S.room) { S.toRoom = S.room; S.toChosen = ""; $("to").value = ""; }
  const senders = new Set();
  for (const m of all()) if (m.from !== S.st.human) senders.add(m.from);
  // A recipient the person chose stays in the list even when that author has
  // fallen out of the loaded history, so a poll cannot turn the message
  // into one for the whole room.
  if (S.toChosen) senders.add(S.toChosen);
  const sig = JSON.stringify([S.room, S.reply && S.reply.from, [...senders]]);
  if (sig === S.toSig) return;
  S.toSig = sig;
  const sel = $("to"), cur = sel.value;
  sel.replaceChildren(el("option", "", S.reply ? escapeDisplay(S.reply.from) + " (reply)" : "Everyone"));
  sel.firstChild.value = "";
  for (const s of senders) { const o = el("option", "", escapeDisplay(s)); o.value = s; sel.append(o); }
  sel.value = cur;
}
$("to").addEventListener("change", () => { S.toChosen = $("to").value; });
function setReply(m) {
  S.reply = m;
  renderHead();
  const r = $("replying");
  r.hidden = false;
  r.replaceChildren("Replying to " + escapeDisplay(m.from) + ": " + escapeDisplay(Array.from(m.text || "").slice(0, 60).join("")) + " ", btn("Cancel reply", "link", () => { S.reply = null; r.hidden = true; renderHead(); $("text").focus(); }));
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
    const machine = (c) => c.id_shared ? [escapeDisplay(c.node), " ", el("span", "mark", "id written in full: short id shared")] : [fp(c.node)];
    for (const c of t.copies || []) ct.append(el("tr", "", el("td", "", ...machine(c)), el("td", "", fmt(c.at)), el("td", "", c.member ? "yes" : "no")));
    ht.append(el("tr", "", ...["Agent", "When"].map((h) => el("th", "", h))));
    const who = (a) => [el("span", "from", escapeDisplay(a.agent || a.from)), ...authorMarks({ ...a, from: a.agent || a.from }).flatMap((k) => [" ", el("span", "mark", k)])];
    for (const h of t.handled || []) ht.append(el("tr", "", el("td", "", ...who(h)), el("td", "", fmt(h.at))));
    // The same words as a2p2 trace (cmd/a2p2, textWords and copyReportWords).
    const textState = {
      live: "available on this machine", missing: "waiting for a copy",
      not_admitted: "outside this machine's permitted room history",
      withdrawn: "withdrawn by its author", expired: "expired (text is kept 30 days)",
    }[t.reason] || t.reason;
    showDialog(el("h2", "", "Message " + fp(id)),
      ...(t.from ? [el("p", "", "From ", ...who(t))] : []),
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
    for (const m of t.entries || []) kids.push(el("div", "thr", entryMeta(m), el("div", "body", escapeText(m.text || m.kind)), filesOf(m, () => openThread(id))));
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
    if (e.message !== "unauthorized" && !signedOut) {
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
  // Never send room-wide a message the person addressed to someone.
  if (S.toChosen && $("to").value !== S.toChosen) {
    $("err").textContent = "The recipient you chose is no longer in the list. Choose who it is for again; nothing was sent.";
    return;
  }
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
(async () => {
  if (code) {
    busy = true; // a refresh waits for the trade
    try { await trade(); } catch (e) { token = ""; showAuthFail("This link did not work: " + e.message); } finally { busy = false; }
  }
  if (!token) { showAuthFail(); return; }
  if (signedOut) return;
  refresh(true); setInterval(refresh, 2000);
})();
