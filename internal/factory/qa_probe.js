(() => {
  // The factory's page probe (magnum, internal/factory/qa_probe.js). The QA
  // walker evaluates this expression in each page it captures and saves the
  // JSON string it returns, unchanged, beside the page's PNG; the factory
  // reads the page from it, never from the walker's word. The nonce shows
  // only that the walker read this run's script, not that the file is what
  // the script returned. The file starts with the expression, so a tool that
  // runs it as `return <script>` gets its value.
  //
  // text is the text the page shows: its text nodes in order, one per line,
  // open shadow roots included. Text under PRE, CODE, KBD, SAMP, TEXTAREA,
  // editable elements and code editors (CodeMirror, Monaco, Ace) is code or
  // input the page shows on purpose, and SCRIPT, STYLE, NOSCRIPT and
  // TEMPLATE are never shown, so they are left out, as is text that is not
  // rendered: no client rects (display none), or visibility not visible. An
  // element with display contents has no box of its own, so
  // checkVisibility() is false on it while its content shows: only an
  // element without a box for another reason is left out whole. html is the
  // page's HTML, copied into an inert document (so no image of it loads),
  // with the contents of its scripts, styles, templates and noscripts
  // emptied. text keeps at most 200000 bytes and html 400000; truncated says
  // one was cut. A page that breaks the probe gets {nonce, url, error}.
  const nonce = "__MAGNUM_NONCE__";
  try {
    const TEXT_CAP = 200000;
    const HTML_CAP = 400000;
    const SKIP = new Set(["PRE", "CODE", "KBD", "SAMP", "TEXTAREA", "SCRIPT", "STYLE", "NOSCRIPT", "TEMPLATE"]);
    const EDITORS = ".cm-editor, .monaco-editor, .ace_editor";
    const encoder = new TextEncoder();
    let truncated = false;

    // cap cuts s to at most n bytes of UTF-8.
    const cap = (s, n) => {
      const bytes = encoder.encode(s);
      if (bytes.length <= n) return s;
      truncated = true;
      return new TextDecoder().decode(bytes.subarray(0, n)).replace(/\uFFFD$/, "");
    };

    // styleOf is the computed style of a text node's parent: its element, or
    // the host of the shadow root it sits in.
    const styleOf = (node) => getComputedStyle(node.parentElement || node.parentNode.host);
    const range = document.createRange();
    const filter = {
      acceptNode(node) {
        if (node.nodeType === Node.TEXT_NODE) {
          if (!node.nodeValue.trim()) return NodeFilter.FILTER_SKIP;
          range.selectNodeContents(node);
          if (range.getClientRects().length === 0 || styleOf(node).visibility !== "visible") return NodeFilter.FILTER_SKIP;
          return NodeFilter.FILTER_ACCEPT;
        }
        if (SKIP.has(node.nodeName.toUpperCase()) || node.isContentEditable || node.matches(EDITORS)) return NodeFilter.FILTER_REJECT;
        if (node.checkVisibility && !node.checkVisibility() && getComputedStyle(node).display !== "contents") return NodeFilter.FILTER_REJECT;
        return NodeFilter.FILTER_ACCEPT;
      },
    };

    // UTF-16 units count no more than UTF-8 bytes, so stopping on them
    // never stops short of the cap.
    const parts = [];
    let units = 0;
    const walk = (root) => {
      const walker = document.createTreeWalker(root, NodeFilter.SHOW_ELEMENT | NodeFilter.SHOW_TEXT, filter);
      for (let node = walker.nextNode(); node && units <= TEXT_CAP; node = walker.nextNode()) {
        if (node.nodeType === Node.TEXT_NODE) {
          parts.push(node.nodeValue);
          units += node.nodeValue.length + 1;
        } else if (node.shadowRoot) {
          walk(node.shadowRoot);
        }
      }
    };
    walk(document.body || document.documentElement);
    const text = cap(parts.join("\n"), TEXT_CAP);

    const copy = document.implementation.createHTMLDocument("").importNode(document.documentElement, true);
    for (const el of copy.querySelectorAll("script, style, template, noscript")) {
      el.replaceChildren();
      if (el.content) el.content.replaceChildren();
    }
    const html = cap(copy.outerHTML, HTML_CAP);

    const root = document.documentElement;
    const clipped = (el) => !!el && ["hidden", "clip"].includes(getComputedStyle(el).overflowX);
    return JSON.stringify({
      nonce,
      url: location.href,
      status: performance.getEntriesByType("navigation")[0]?.responseStatus || 0,
      title: document.title,
      overflow_x: root.scrollWidth > root.clientWidth + 1 && !clipped(root) && !clipped(document.body),
      text,
      html,
      truncated,
    });
  } catch (e) {
    return JSON.stringify({ nonce, url: String(location.href), error: String((e && e.message) || e || "probe error").slice(0, 500) });
  }
})()
