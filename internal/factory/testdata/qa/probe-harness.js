"use strict";
// probe-harness.js runs the factory's page probe (internal/factory/qa_probe.js)
// in Node against a fake DOM, for the factory's tests: no browser.
//
//   node probe-harness.js <probe.js> <page.dom.json> [--function-body]
//
// It prints what the probe returns. --function-body evaluates the probe as a
// tool does that wraps it in `return <script>` (a function body).
//
// The fake DOM follows the browser's rules the probe relies on:
//   - checkVisibility() is false for an element without a box: display none on
//     it or an ancestor, or display contents on it alone (WPT
//     css/cssom-view/checkVisibility.html); with visibilityProperty, also for a
//     computed visibility other than visible;
//   - a Range over a text node has client rects unless an ancestor has
//     display none;
//   - visibility inherits; a TreeWalker never enters a shadow root, and
//     FILTER_REJECT skips a node with its subtree, FILTER_SKIP the node alone.
// The page is a JSON tree: a node is a string (text) or {tag, attrs, style,
// children, shadow}; the page has url, status, title, scrollWidth,
// clientWidth, throw (createTreeWalker throws) and html (the root element).

const fs = require("fs");
const vm = require("vm");

const [, , probePath, pagePath, mode] = process.argv;
const page = JSON.parse(fs.readFileSync(pagePath, "utf8"));

const ELEMENT_NODE = 1;
const TEXT_NODE = 3;
const FRAGMENT_NODE = 11;
const VOID = new Set(["BR", "HR", "IMG", "INPUT", "META", "LINK"]);

class FakeNode {
  constructor(type, name) {
    this.nodeType = type;
    this.nodeName = name;
    this.parentNode = null;
    this.childNodes = [];
  }
  get parentElement() {
    return this.parentNode && this.parentNode.nodeType === ELEMENT_NODE ? this.parentNode : null;
  }
  append(child) {
    child.parentNode = this;
    this.childNodes.push(child);
  }
  replaceChildren() {
    this.childNodes = [];
  }
}

class FakeText extends FakeNode {
  constructor(value) {
    super(TEXT_NODE, "#text");
    this.nodeValue = value;
  }
  cloneNode() {
    return new FakeText(this.nodeValue);
  }
  get html() {
    return this.nodeValue.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  }
}

class FakeFragment extends FakeNode {
  constructor(host) {
    super(FRAGMENT_NODE, "#document-fragment");
    this.host = host;
  }
}

class FakeElement extends FakeNode {
  constructor(tag, attrs, style) {
    super(ELEMENT_NODE, tag.toUpperCase());
    this.attrs = { ...(attrs || {}) };
    this.style = { ...(style || {}) };
    this.shadowRoot = null;
    this.content = this.nodeName === "TEMPLATE" ? new FakeFragment(null) : undefined;
  }
  get isContentEditable() {
    for (let e = this; e; e = flatParent(e)) {
      const v = e.attrs && e.attrs.contenteditable;
      if (v === "" || v === "true") return true;
      if (v === "false") return false;
    }
    return false;
  }
  matches(selectors) {
    return selectors.split(",").some((raw) => {
      const s = raw.trim();
      if (s.startsWith(".")) return (this.attrs.class || "").split(/\s+/).includes(s.slice(1));
      return this.nodeName === s.toUpperCase();
    });
  }
  querySelectorAll(selectors) {
    const found = [];
    const visit = (n) => {
      for (const c of n.childNodes) {
        if (c.nodeType !== ELEMENT_NODE) continue;
        if (c.matches(selectors)) found.push(c);
        visit(c);
      }
    };
    visit(this);
    return found;
  }
  cloneNode(deep) {
    const e = new FakeElement(this.nodeName, this.attrs, this.style);
    if (deep) {
      for (const c of this.childNodes) e.append(c.cloneNode(true));
      if (this.content) for (const c of this.content.childNodes) e.content.append(c.cloneNode(true));
    }
    return e;
  }
  checkVisibility(options) {
    for (let e = this; e; e = flatParent(e)) if (e.style.display === "none") return false;
    if (this.style.display === "contents") return false;
    const o = options || {};
    if ((o.visibilityProperty || o.checkVisibilityCSS) && visibilityOf(this) !== "visible") return false;
    return true;
  }
  get outerHTML() {
    const tag = this.nodeName.toLowerCase();
    const attrs = Object.entries(this.attrs).map(([k, v]) => ` ${k}="${String(v).replace(/&/g, "&amp;").replace(/"/g, "&quot;")}"`).join("");
    if (VOID.has(this.nodeName)) return `<${tag}${attrs}>`;
    const kids = (this.content || this).childNodes.map((c) => (c.nodeType === TEXT_NODE ? c.html : c.outerHTML)).join("");
    return `<${tag}${attrs}>${kids}</${tag}>`;
  }
}

// flatParent is a node's parent in the flat tree: a shadow root's host.
function flatParent(n) {
  const p = n.parentNode;
  return p && p.nodeType === FRAGMENT_NODE ? p.host : p;
}

function visibilityOf(el) {
  for (let e = el; e; e = flatParent(e)) if (e.style && e.style.visibility) return e.style.visibility;
  return "visible";
}

function build(spec) {
  if (typeof spec === "string") return new FakeText(spec);
  const e = new FakeElement(spec.tag, spec.attrs, spec.style);
  for (const c of spec.children || []) (e.content || e).append(build(c));
  if (spec.shadow) {
    e.shadowRoot = new FakeFragment(e);
    for (const c of spec.shadow) e.shadowRoot.append(build(c));
  }
  return e;
}

const root = build(page.html);
root.scrollWidth = page.scrollWidth || 1280;
root.clientWidth = page.clientWidth || 1280;

const NodeFilter = { FILTER_ACCEPT: 1, FILTER_REJECT: 2, FILTER_SKIP: 3, SHOW_ELEMENT: 0x1, SHOW_TEXT: 0x4 };

function createTreeWalker(start, show, filter) {
  if (page.throw) throw new Error("the page broke the probe");
  if (page.throwEmpty) throw "";
  function* visit(n) {
    for (const c of n.childNodes) {
      const bit = c.nodeType === ELEMENT_NODE ? NodeFilter.SHOW_ELEMENT : c.nodeType === TEXT_NODE ? NodeFilter.SHOW_TEXT : 0;
      const verdict = show & bit ? filter.acceptNode(c) : NodeFilter.FILTER_SKIP;
      if (verdict === NodeFilter.FILTER_REJECT) continue;
      if (verdict === NodeFilter.FILTER_ACCEPT) yield c;
      yield* visit(c);
    }
  }
  const it = visit(start);
  return { nextNode: () => it.next().value || null };
}

function createRange() {
  let node = null;
  return {
    selectNodeContents(n) {
      node = n;
    },
    getClientRects() {
      if (!node || node.nodeType !== TEXT_NODE || node.nodeValue === "") return [];
      for (let e = flatParent(node); e; e = flatParent(e)) if (e.style && e.style.display === "none") return [];
      return [{ width: 1, height: 1 }];
    },
  };
}

const document = {
  documentElement: root,
  body: root.childNodes.find((c) => c.nodeName === "BODY") || null,
  title: page.title || "",
  createTreeWalker,
  createRange,
  implementation: { createHTMLDocument: () => ({ importNode: (n, deep) => n.cloneNode(deep) }) },
};

const context = vm.createContext({
  document,
  Node: { ELEMENT_NODE, TEXT_NODE },
  NodeFilter,
  getComputedStyle: (el) => ({
    display: el.style.display || "block",
    visibility: visibilityOf(el),
    overflowX: el.style.overflowX || "visible",
  }),
  location: { href: page.url },
  performance: { getEntriesByType: () => [{ responseStatus: page.status || 0 }] },
  TextEncoder,
  TextDecoder,
});

const script = fs.readFileSync(probePath, "utf8");
const result = mode === "--function-body" ? vm.runInContext(`(function () { return ${script}\n})()`, context) : vm.runInContext(script, context);
process.stdout.write(String(result));
