// Runs static/app.js in Node against a real a2p2 web handler.
//
//   node page_harness.js INDEX_HTML APP_JS BASE_URL TOKEN ROOM_ID
//
// Node has no DOM, so a small stand-in supplies the parts app.js uses. Its
// elements are built from INDEX_HTML: an id the HTML does not define is null
// from getElementById, as in a browser, and the page's script tag must name
// /app.js. It keeps what the script draws as text; it does not lay anything
// out, run CSS or fire events by itself. Every call to the server is real:
// fetch goes to BASE_URL with the token, so the data has the shape the daemon
// sends. After the first refresh it drives one scenario by calling the
// listeners the script registered: open ROOM_ID, choose an author as the
// recipient, reload with that author gone from the history, and submit.
// Prints one JSON object on stdout with what the test asserts on.
"use strict";
const fs = require("fs");
const vm = require("vm");
const [htmlPath, appPath, base, token, roomId] = process.argv.slice(2);
const errors = [];

class Text {
  constructor(s) { this.nodeType = 3; this.data = s; this.parentNode = null; }
  get textContent() { return this.data; }
}
class Element {
  constructor(tag) {
    this.nodeType = 1; this.tagName = String(tag).toUpperCase(); this.children = [];
    this.parentNode = null; this.dataset = {}; this.attrs = {}; this.listeners = {};
    this.className = ""; this.hidden = false; this.disabled = false; this.open = false;
    this.title = ""; this.type = ""; this.id = ""; this.scrollTop = 0; this.scrollHeight = 0;
    this.placeholder = ""; this.style = {};
    this._value = "";
    this.classList = { add() {}, remove() {}, toggle() {}, contains: () => false };
  }
  get value() { return this._value; }
  set value(v) { this._value = v; }
  _adopt(k) {
    if (!k.nodeType) return new Text(String(k));
    if (k.parentNode) k.parentNode.children = k.parentNode.children.filter((c) => c !== k);
    k.parentNode = this;
    return k;
  }
  append(...kids) { for (const k of kids) this.children.push(this._adopt(k)); this._changed(); }
  appendChild(k) { this.append(k); return k; }
  replaceChildren(...kids) { this.children = []; this.append(...kids); }
  _changed() {}
  get firstChild() { return this.children[0] || null; }
  get textContent() { return this.children.map((c) => c.textContent).join(""); }
  set textContent(s) { this.children = s === "" ? [] : [new Text(String(s))]; this._changed(); }
  contains(o) { for (let n = o; n; n = n.parentNode) if (n === this) return true; return false; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; }
  addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); }
  removeEventListener() {}
  focus() {} blur() {} scrollIntoView() {} showModal() { this.open = true; } close() { this.open = false; }
  _all(f, out = []) { for (const c of this.children) if (c.nodeType === 1) { if (f(c)) out.push(c); c._all(f, out); } return out; }
  querySelectorAll(sel) {
    // tag, .class, tag.class or tag[attr=value]: all the script uses. Anything else is an error.
    const m = /^([a-z0-9]*)(?:\.([\w-]+))?(?:\[(\w+)=([^\]]+)\])?$/.exec(sel);
    if (!m) throw new Error("harness: unsupported selector " + sel);
    return this._all((c) => (!m[1] || c.tagName === m[1].toUpperCase()) &&
      (!m[2] || c.className.split(/\s+/).includes(m[2])) &&
      (!m[3] || (c.attrs[m[3]] ?? c[m[3]]) === m[4]));
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  dispatch(ev, extra) {
    const e = Object.assign({ type: ev, preventDefault() {}, stopPropagation() {}, target: this }, extra);
    for (const fn of this.listeners[ev] || []) fn(e);
  }
}
class Option extends Element {
  constructor() { super("option"); this._v = null; }
  get value() { return this._v !== null ? this._v : this.textContent; }
  set value(v) { this._v = String(v); }
}
// A select as a browser has it: the first option is selected when there is
// none; setting a value no option has selects nothing, and value is then "".
class Select extends Element {
  constructor() { super("select"); this._idx = -1; this._cleared = false; }
  get options() { return this.children.filter((c) => c.tagName === "OPTION"); }
  _changed() { if (this._idx < 0 && !this._cleared && this.options.length) this._idx = 0; }
  replaceChildren(...kids) { this.children = []; this._idx = -1; this._cleared = false; this.append(...kids); }
  get value() { const o = this.options[this._idx]; return o ? o.value : ""; }
  set value(v) { this._idx = this.options.findIndex((o) => o.value === v); this._cleared = this._idx < 0; }
  get selectedIndex() { return this._idx; }
}
const make = (tag) => (tag === "select" ? new Select() : tag === "option" ? new Option() : new Element(tag));

// ---- the page's own structure, from index.html ----
const html = fs.readFileSync(htmlPath, "utf8");
const byId = new Map();
const root = new Element("html");
const scripts = [];
{
  const VOID = new Set(["meta", "link", "input", "br", "img", "hr"]);
  const tok = /<!--[\s\S]*?-->|<!doctype[^>]*>|<\/(\w+)\s*>|<(\w+)((?:\s+[^\s=>\/]+(?:=(?:"[^"]*"|'[^']*'|[^\s>]+))?)*)\s*\/?>|([^<]+)/gi;
  const stack = [root];
  let m;
  while ((m = tok.exec(html))) {
    if (m[1]) { const t = m[1].toLowerCase(); while (stack.length > 1 && stack.pop().tagName.toLowerCase() !== t); continue; }
    if (m[4] !== undefined) { if (m[4].trim()) stack[stack.length - 1].append(new Text(m[4].replace(/\s+/g, " "))); continue; }
    if (!m[2]) continue;
    const tag = m[2].toLowerCase(), e = make(tag);
    for (const a of m[3].matchAll(/([^\s=]+)(?:=(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?/g)) {
      const v = a[2] ?? a[3] ?? a[4] ?? "";
      e.attrs[a[1]] = v;
      if (a[1] === "id") { e.id = v; byId.set(v, e); }
      if (a[1] === "class") e.className = v;
      if (a[1] === "hidden") e.hidden = true;
      if (a[1] === "type") e.type = v;
    }
    if (tag === "script" && e.attrs.src) scripts.push(e.attrs.src);
    stack[stack.length - 1].append(e);
    if (!VOID.has(tag) && !/\/>$/.test(m[0])) stack.push(e);
  }
}
if (!scripts.includes("/app.js")) errors.push("index.html has no <script src=\"/app.js\">");
const posts = [];
let dropHistory = false;
const document = {
  activeElement: null,
  body: root.querySelector("body") || new Element("body"),
  getElementById: (id) => byId.get(id) || null,
  createElement: (t) => make(String(t).toLowerCase()),
  createTextNode: (s) => new Text(s),
  querySelector: (sel) => root.querySelector(sel),
  addEventListener() {},
};
const $ = (id) => { const e = document.getElementById(id); if (!e) throw new Error("index.html has no id " + id); return e; };

const win = {
  document,
  location: { hash: "#" + encodeURIComponent(token), pathname: "/", search: "" },
  history: { replaceState() {} },
  sessionStorage: { getItem: () => null, setItem() {} },
  matchMedia: () => ({ matches: true, addEventListener() {} }),
  requestAnimationFrame: (f) => setTimeout(f, 0),
  fetch: (p, o) => {
    const path = String(p);
    if (o && o.method === "POST" && path.startsWith("/api/send")) posts.push(JSON.parse(o.body));
    if (dropHistory && path.startsWith("/api/history")) return Promise.resolve(Response.json({ entries: [] }));
    return fetch(new URL(path, base), o);
  },
  setInterval: () => 0, // the script's own polling is not started; the scenario calls refresh
  setTimeout, clearTimeout, console, Date, JSON, Promise, Set, Map, Math, URL,
  Error, String, Number, Array, Object, RegExp, Boolean, isNaN, parseInt,
  encodeURIComponent, decodeURIComponent,
};
win.window = win;
process.on("uncaughtException", (e) => errors.push("uncaught: " + e.message));
process.on("unhandledRejection", (e) => errors.push("unhandled: " + (e && e.message)));

const ctx = vm.createContext(win);
const inPage = (src) => vm.runInContext(src, ctx);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// Waits for the page's refresh to end, by drawing or by failing; hitting the
// deadline is an error, never a normal end.
async function idle(what) {
  const deadline = Date.now() + 15000;
  for (;;) {
    if (!inPage("busy")) return true;
    if (Date.now() > deadline) { errors.push("deadline: " + what + " did not finish in 15 seconds"); return false; }
    await sleep(20);
  }
}

(async () => {
  const out = { errors, scenario: {} };
  try {
    out.appHiddenBefore = $("app").hidden;
    vm.runInContext(fs.readFileSync(appPath, "utf8"), ctx, { filename: "app.js" });
    await idle("the first refresh");
    await sleep(100);
    out.connectionHidden = $("connection").hidden;
    out.connection = $("connection").hidden ? "" : $("connection").textContent;
    out.appHidden = $("app").hidden;
    out.roomIds = $("rooms").children.map((li) => li.firstChild.dataset.room);
    out.rooms = $("rooms").children.map((li) => li.textContent);
    out.node = $("node").textContent;

    // The scenario: open the room, address its author, lose the author from the history, send.
    const button = $("rooms").children.map((li) => li.firstChild).find((b) => b.dataset.room === roomId);
    if (!button) throw new Error("no sidebar button for room " + roomId);
    button.dispatch("click");
    await sleep(50); await idle("opening the room"); await sleep(100);
    out.roomhead = $("roomhead").textContent;
    out.stream = $("msgs").textContent;
    const sel = $("to");
    out.toOptions = sel.options.map((o) => o.value);
    const author = out.toOptions.find((v) => v !== "");
    if (author) {
      sel.value = author;
      sel.dispatch("change");
      dropHistory = true;
      inPage("refresh(true)");
      await sleep(50); await idle("the reload without the author"); await sleep(100);
      out.scenario.toAfterReload = sel.value;
      $("text").value = "a private note";
      $("compose").dispatch("submit");
      await sleep(300); await idle("the send");
      out.scenario.author = author;
      out.scenario.posts = posts.slice();
      // A recipient lost some other way: the page must refuse, not send room-wide.
      posts.length = 0;
      sel.value = "";
      $("text").value = "second note";
      $("compose").dispatch("submit");
      await sleep(300);
      out.scenario.refusedPosts = posts.slice();
      out.scenario.refusedErr = $("err").textContent;
    }
  } catch (e) {
    errors.push("harness: " + e.message);
  }
  process.stdout.write(JSON.stringify(out) + "\n");
  process.exit(0);
})();
