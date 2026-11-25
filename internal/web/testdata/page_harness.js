// Runs static/app.js in Node against a real a2p2 web handler.
//
//   node page_harness.js APP_JS BASE_URL TOKEN
//
// Node has no DOM, so a small stand-in supplies the parts app.js uses. It is
// only enough to execute the script and keep what it draws as text; it does
// not lay anything out. Every call to the server is real: fetch goes to
// BASE_URL with the token, so the data has the shape the daemon sends.
// Prints one JSON object on stdout: the errors seen, the text of the status
// line, and the text of the sidebar's rooms, the room head and the stream.
"use strict";
const fs = require("fs");
const vm = require("vm");
const [appPath, base, token] = process.argv.slice(2);

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
    this.value = ""; this.title = ""; this.type = ""; this.id = ""; this.scrollTop = 0; this.scrollHeight = 0;
    this.classList = { add() {}, remove() {}, toggle() {}, contains: () => false };
    this.style = {};
  }
  _adopt(k) { if (k.nodeType) { if (k.parentNode) k.parentNode.children = k.parentNode.children.filter((c) => c !== k); k.parentNode = this; return k; } return new Text(String(k)); }
  append(...kids) { for (const k of kids) this.children.push(this._adopt(typeof k === "object" && k ? k : new Text(String(k)))); }
  appendChild(k) { this.append(k); return k; }
  replaceChildren(...kids) { this.children = []; this.append(...kids); }
  get firstChild() { return this.children[0] || null; }
  get textContent() { return this.children.map((c) => c.textContent).join(""); }
  set textContent(s) { this.children = s === "" ? [] : [new Text(String(s))]; }
  contains(o) { for (let n = o; n; n = n.parentNode) if (n === this) return true; return false; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; }
  addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); }
  removeEventListener() {}
  focus() {} blur() {} scrollIntoView() {} showModal() { this.open = true; } close() { this.open = false; }
  _all(f, out = []) { for (const c of this.children) if (c.nodeType === 1) { if (f(c)) out.push(c); c._all(f, out); } return out; }
  querySelectorAll(sel) { const t = sel.split("[")[0].toUpperCase(); return this._all((c) => c.tagName === t); }
  // The form's submit button is in index.html, which this stand-in does not load.
  querySelector(sel) { return this.querySelectorAll(sel)[0] || (sel === "button[type=submit]" ? new Element("button") : null); }
}

const byId = new Map();
const document = {
  activeElement: null,
  body: new Element("body"),
  getElementById(id) {
    if (!byId.has(id)) { const e = new Element("div"); e.id = id; byId.set(id, e); }
    return byId.get(id);
  },
  createElement: (t) => new Element(t),
  createTextNode: (s) => new Text(s),
  querySelector(sel) { return sel === ".machine-details" ? (this._md = this._md || new Element("details")) : null; },
  addEventListener() {},
};
// Starts hidden, as in index.html: the script must show it.
const conn = document.getElementById("connection");
conn.hidden = true;

const win = {
  document,
  location: { hash: "#" + encodeURIComponent(token), pathname: "/", search: "" },
  history: { replaceState() {} },
  sessionStorage: { getItem: () => null, setItem() {} },
  matchMedia: () => ({ matches: true, addEventListener() {} }),
  requestAnimationFrame: (f) => setTimeout(f, 0),
  fetch: (p, o) => fetch(new URL(p, base), o),
  setInterval: () => 0, // one refresh is enough; no timer keeps the process alive
  setTimeout, clearTimeout, console, Date, JSON, Promise, Set, Map, Math, URL,
  Error, String, Number, Array, Object, RegExp, Boolean, isNaN, parseInt,
  encodeURIComponent, decodeURIComponent,
};
win.window = win;
// An error thrown inside a handler is logged by app.js itself, so ask for the
// message it shows as well as for uncaught ones.
process.on("uncaughtException", (e) => errors.push("uncaught: " + e.message));
process.on("unhandledRejection", (e) => errors.push("unhandled: " + (e && e.message)));

const ctx = vm.createContext(win);
try {
  vm.runInContext(fs.readFileSync(appPath, "utf8"), ctx, { filename: "app.js" });
} catch (e) {
  errors.push("load: " + e.message);
}

// Wait for the first refresh to end. app.js calls refresh(true) as it loads,
// so busy is true until that run finishes, by drawing the page or by failing.
const deadline = Date.now() + 15000;
(function wait() {
  let busy = true;
  try { busy = vm.runInContext("busy", ctx); } catch (e) { errors.push("busy: " + e.message); busy = false; }
  if (!busy || Date.now() > deadline) return done();
  setTimeout(wait, 25);
})();
function done() {
  setTimeout(() => {
    const out = {
      errors,
      connection: conn.hidden ? "" : conn.textContent,
      appHidden: document.getElementById("app").hidden,
      rooms: document.getElementById("rooms").children.map((li) => li.textContent),
      roomhead: document.getElementById("roomhead").textContent,
      stream: document.getElementById("msgs").textContent,
      node: document.getElementById("node").textContent,
    };
    process.stdout.write(JSON.stringify(out) + "\n");
    process.exit(0);
  }, 100);
}
