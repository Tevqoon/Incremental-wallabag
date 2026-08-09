// increader — the only hand-written JavaScript in the project.
//
// Its main job is translating a text selection into the coordinates the server
// addresses passages by: a block index (the data-b attribute the server put on
// every paragraph) plus a character offset into that block's text. Everything
// else is htmx, except the theme toggle at the bottom, which is too small a
// job to justify a second file.

(function () {
  "use strict";

  // The selection captured when the toolbar appeared. It has to be stored,
  // because clicking a button collapses the selection before htmx reads it.
  let captured = null;

  // The Range the toolbar is currently pinned to, kept separately from
  // `captured`: that only holds the server-facing block/offset coordinates,
  // but repositioning needs a Range's getBoundingClientRect(), which is
  // recomputed from wherever its endpoints currently sit in the page. A
  // one-off {top, left} snapshot could not tell the toolbar moved.
  //
  // It must be a *clone* of the selection's Range, never the live one
  // getRangeAt(0) hands back — see showToolbar. A clone still tracks DOM
  // reflow (an image loading, MathJax retypesetting) exactly like the live
  // Range would, which is the property this needs; what it must not do is
  // track the selection itself collapsing, which the live Range would.
  let toolbarRange = null;

  const toolbar = () => document.getElementById("selection-toolbar");

  // ---- Layout settling ----------------------------------------------------
  //
  // Three unrelated features below all fight the same problem: something
  // this page cares about — the read point to scroll to, the article's
  // scroll anchor, the selection toolbar's position — gets computed once,
  // and then late-loading content moves it. The late loader is never the
  // same thing twice (an <img> the server could not size because it is SVG
  // or AVIF, MathJax's async typeset, a phone rotating), so rather than wire
  // up a bespoke listener for each, settleLayout just polls: it re-runs
  // `correct` on a short interval for up to `budgetMs`, and stops for good
  // — sooner than the budget, if `stopped()` says so first. `correct` must
  // be safe to call redundantly — every caller's version already is, since
  // "already in the right place" is a no-op for all three.
  function settleLayout(budgetMs, correct, stopped) {
    const deadline = Date.now() + budgetMs;
    (function tick() {
      if (stopped()) return;
      correct();
      if (Date.now() < deadline) setTimeout(tick, 120);
    })();
  }

  // Tells a scroll this file caused itself apart from one the user just
  // made — the two pinning features below (the resume scroll and the
  // post-extract anchor) need that distinction before they are allowed to
  // touch scroll position again, and a `scroll` event alone cannot make it,
  // since a corrective scroll fires the identical event. Every corrective
  // scroll must go through `own()`, which raises a flag the listener treats
  // as "not the user" and lowers again next frame — one frame is enough
  // because neither caller here scrolls smoothly, so the resulting `scroll`
  // event has already landed by then. A touch or a wheel spin counts as the
  // user taking over immediately, before it has even produced a `scroll`
  // event, rather than waiting a frame to find out.
  function scrollGuard() {
    let ownScroll = false;
    let interrupted = false;

    function mark() {
      if (interrupted) return;
      interrupted = true;
      teardown();
    }
    function onScroll() {
      if (!ownScroll) mark();
    }

    window.addEventListener("scroll", onScroll, { passive: true });
    window.addEventListener("touchstart", mark, { passive: true });
    window.addEventListener("wheel", mark, { passive: true });
    window.addEventListener("keydown", mark);

    function teardown() {
      window.removeEventListener("scroll", onScroll);
      window.removeEventListener("touchstart", mark);
      window.removeEventListener("wheel", mark);
      window.removeEventListener("keydown", mark);
    }

    return {
      own(fn) {
        ownScroll = true;
        fn();
        requestAnimationFrame(() => { ownScroll = false; });
      },
      interrupted: () => interrupted,
      dispose: teardown,
    };
  }

  /**
   * Keeps `scrollFn` correcting the scroll position for up to `budgetMs`,
   * stopping instantly and for good the moment the user scrolls, touches, or
   * otherwise interacts (see scrollGuard) — fighting a user's own scroll is
   * worse than any of the layout jumps this exists to fix. Returns a
   * `correct` function callers can also invoke directly from their own
   * "something finished loading" hooks (an image's load event, a promise
   * resolving) for a same-frame correction instead of waiting on the next
   * poll tick; it stays safe to call even after the budget or an
   * interruption, since it re-checks both itself rather than trusting the
   * caller to stop asking.
   */
  function pinScroll(scrollFn, budgetMs) {
    const guard = scrollGuard();
    const deadline = Date.now() + budgetMs;

    function correct() {
      if (guard.interrupted() || Date.now() > deadline) return;
      guard.own(scrollFn);
    }

    settleLayout(budgetMs, correct, guard.interrupted);
    setTimeout(guard.dispose, budgetMs);
    return correct;
  }

  /**
   * Characters of `block`'s text that come before the point (container, offset).
   *
   * A Range from the start of the block to that point stringifies to exactly
   * the text in between, so its length is the offset. This works whether the
   * boundary landed in a text node or between elements, which hand-counting
   * text nodes does not.
   *
   * It also matches the server's counting for free: <mark> elements from
   * earlier extracts contain real text and are counted by both sides.
   */
  function offsetWithin(block, container, offset) {
    const measure = document.createRange();
    measure.selectNodeContents(block);
    try {
      measure.setEnd(container, offset);
    } catch (e) {
      return 0;
    }
    return measure.toString().length;
  }

  /** The addressable block containing a node, or null if there is none. */
  function blockOf(node) {
    const element = node.nodeType === Node.TEXT_NODE ? node.parentElement : node;
    return element ? element.closest("[data-b]") : null;
  }

  /** Reads the current selection into the coordinates the server expects. */
  function readSelection() {
    const selection = window.getSelection();
    if (!selection || selection.isCollapsed || selection.rangeCount === 0) {
      return null;
    }

    const range = selection.getRangeAt(0);
    const startBlock = blockOf(range.startContainer);
    const endBlock = blockOf(range.endContainer);
    if (!startBlock || !endBlock) {
      return null; // Selection strayed outside the article.
    }

    const payload = {
      start_block: parseInt(startBlock.dataset.b, 10),
      start_offset: offsetWithin(startBlock, range.startContainer, range.startOffset),
      end_block: parseInt(endBlock.dataset.b, 10),
      end_offset: offsetWithin(endBlock, range.endContainer, range.endOffset),
      quote: selection.toString(),
    };

    if (payload.quote.trim() === "") {
      return null;
    }
    return payload;
  }

  // htmx calls these through hx-vals="js:...".
  window.selectionPayload = () => captured || {};

  // A cloze is stored as offsets into the extract's flat text, but the browser
  // only knows block coordinates. Send those and let the server convert: it
  // has the extract's block structure and can account for the separators
  // between paragraphs, which the browser cannot see.
  window.clozePayload = () => captured || {};

  window.clearSelection = () => {
    captured = null;
    toolbarRange = null;
    const bar = toolbar();
    if (bar) bar.hidden = true;
    window.getSelection().removeAllRanges();
  };

  /** Places the toolbar just above (or, if there is no room, below) toolbarRange. */
  function positionToolbar() {
    const bar = toolbar();
    if (!bar || bar.hidden || !toolbarRange) return;

    const box = toolbarRange.getBoundingClientRect();

    // A zero-size rect means there is nothing to point at — toolbarRange
    // collapsed or its content left the DOM. Cloning the Range in
    // showToolbar is what keeps a selection collapsing (the iOS tap that is
    // about to press this very toolbar) from doing that; this is only a
    // backstop against some other route to the same state, not something
    // expected to fire in practice, so it just leaves the toolbar where it
    // last was rather than snapping it to a caret.
    if (box.width === 0 && box.height === 0) return;

    const top = box.top + window.scrollY - bar.offsetHeight - 8;

    // Below the selection when there is no room above it, rather than off the
    // top of the page.
    bar.style.top = `${top < window.scrollY ? box.bottom + window.scrollY + 8 : top}px`;

    // Keep it on screen when the selection starts near the right edge.
    const left = Math.min(box.left + window.scrollX,
                          document.documentElement.clientWidth - bar.offsetWidth - 8);
    bar.style.left = `${Math.max(8, left)}px`;
  }

  /** Shows the toolbar just above the selected text. */
  function showToolbar(range) {
    const bar = toolbar();
    if (!bar) return;

    // Cloned, not stored as-is: `range` is window.getSelection().getRangeAt(0)
    // — the selection's own live Range — and on iOS the tap meant to press
    // Extract is usually what collapses the selection (see the big comment
    // on the selectionchange handler below). If toolbarRange were that same
    // live Range, the settling poll a few lines down would call
    // getBoundingClientRect() on it mid-tap, get back a zero-size rect at
    // the caret the selection collapsed to, and yank the toolbar out from
    // under the finger pressing it — reintroducing exactly the failure that
    // comment describes, through the position instead of the visibility. A
    // clone is detached from the selection, so it cannot collapse under it,
    // but it still recomputes its rect from current layout on every call,
    // which is the actual thing this needs: following the text as images
    // load and the page reflows underneath it.
    toolbarRange = range.cloneRange();

    // Unhide before measuring: offsetHeight is 0 while the element is hidden,
    // which would place the toolbar on top of the text instead of above it.
    bar.hidden = false;
    positionToolbar();

    // An image above the selection finishing its load, or MathJax
    // retypesetting, moves the selected text out from under a toolbar placed
    // only once; keep re-placing it for a bounded settling window. This one
    // stops on "the toolbar went away" rather than "the user scrolled" —
    // moving alongside a scroll is exactly what should happen here, and it
    // is positionToolbar reading live scrollY/getBoundingClientRect each
    // time, not a pinned scroll position, that makes that automatic.
    settleLayout(2000, positionToolbar, () => bar.hidden);
  }

  // Resize or rotate at any point while the toolbar is up — not just right
  // after it appears, which settleLayout's short window above does not
  // cover — and it has to follow. positionToolbar is a no-op whenever the
  // toolbar is hidden, so this is harmless to leave attached for the page's
  // whole life rather than adding and removing it around every show/hide.
  window.addEventListener("resize", positionToolbar);
  window.addEventListener("orientationchange", positionToolbar);

  document.addEventListener("selectionchange", () => {
    const payload = readSelection();
    if (!toolbar()) return;

    // A collapse is ignored, not treated as "cancel". On iOS the very tap
    // meant to press Extract or Cloze is usually what collapses the
    // selection: WebKit uses the first touch outside the selection to
    // dismiss its own selection UI, frequently without ever delivering a
    // click to the page at all. Hiding the toolbar here would pull it out
    // from under the tap that was supposed to use it. So the toolbar and
    // `captured` now only go away for an explicit reason — Extract, Cloze,
    // or the × button all route through clearSelection().
    if (!payload) return;

    captured = payload;
    showToolbar(window.getSelection().getRangeAt(0));
  });

  // ---- Reading position -------------------------------------------------

  /** The index of the topmost block still visible in the viewport. */
  function topVisibleBlock() {
    const blocks = document.querySelectorAll("#article [data-b]");
    let last = null;
    for (const block of blocks) {
      if (block.getBoundingClientRect().bottom > 0) {
        return parseInt(block.dataset.b, 10);
      }
      last = block;
    }

    // Every block has scrolled above the viewport. That is easy to reach
    // just by reading to the end: #article is followed by a clozes section,
    // an "Extracts from this" list, and a sticky grading footer, none of
    // which carry a data-b, so simply scrolling down to read them (or an
    // article ending in a large image, which pushes the last block up and
    // out on its own) puts every [data-b] above the top of the viewport.
    // Falling through to 0 here would be indistinguishable from "still at
    // the very top of the article" to both the throttled tracker below,
    // which would then POST block 0 and reset the saved read point, and to
    // window.topBlock, which the grade buttons read at click time — so
    // pressing Next from the bottom of the page would record "got to the
    // start" as where reading stopped. Scrolled past everything means read
    // to the end, so the last block is what "0" was standing in for; 0 is
    // only honest when there are no blocks at all.
    return last ? parseInt(last.dataset.b, 10) : 0;
  }

  // Exposed for the grading buttons, which send the read point with the grade.
  // Reading it at click time rather than trusting the throttled scroll tracker
  // matters: the tracker can be a second behind, and pressing a grade button is
  // exactly the moment that staleness would be recorded as your place.
  window.topBlock = topVisibleBlock;

  function trackReadingPosition() {
    const reader = document.querySelector(".reader");
    if (!reader) return;

    // Resume where reading stopped. Doing this before wiring up the scroll
    // listener avoids recording the jump itself as progress.
    //
    // The server has already marked that block with .read-point, so the reader
    // sees the boundary between what they have read and what they have not,
    // rather than just arriving mysteriously partway down.
    const resumeAt = parseInt(reader.dataset.readBlock, 10);
    if (resumeAt > 0) {
      const target = document.querySelector(`#article [data-b="${resumeAt}"]`);
      // scrollIntoView here runs before any image below the fold has loaded,
      // so on an image-heavy article it lands short: everything above
      // `target` is still zero-height, and then grows underneath it. Keep
      // correcting for a couple of seconds — pinScroll backs off the instant
      // the reader scrolls, touches, or otherwise takes over, so this can
      // only ever help, never fight them.
      if (target) pinScroll(() => target.scrollIntoView({ block: "start" }), 2000);
    }

    const elementID = reader.dataset.element;
    let lastSent = resumeAt;
    let pending = null;

    // Throttled rather than fired per scroll event: this writes to the
    // database, and a smooth scroll produces hundreds of events a second.
    window.addEventListener("scroll", () => {
      if (pending) return;
      pending = setTimeout(() => {
        pending = null;
        const block = topVisibleBlock();
        if (block === lastSent) return;
        lastSent = block;

        const body = new URLSearchParams({ block: String(block) });
        // keepalive lets the last update survive the page being closed, which
        // is exactly when the final reading position matters most.
        fetch(`/elements/${elementID}/progress`, {
          method: "POST",
          body,
          keepalive: true,
        }).catch(() => {});
      }, 1000);
    });
  }

  document.addEventListener("DOMContentLoaded", trackReadingPosition);

  // ---- Library bulk selection --------------------------------------------
  //
  // The bulk action bar's buttons are ordinary form submits — see
  // library.html's bulk-form — so mass "queue it" and friends already work
  // with no JavaScript at all: check some rows, press a button, land back on
  // this same list. Everything here is a convenience layered on top, not a
  // requirement for the feature to function: a running count so a press of
  // Done or Dismiss is not a leap of faith, "select all" to avoid clicking
  // every row by hand, and a confirmation prompt for the actions marked
  // dangerous enough to want one.

  function initBulkSelection() {
    const selectAll = document.getElementById("select-all");
    const count = document.getElementById("bulk-count");
    if (!selectAll && !count) return; // not the library page

    const boxes = () => document.querySelectorAll('input[name="ids"]');

    function updateCount() {
      if (!count) return;
      const n = document.querySelectorAll('input[name="ids"]:checked').length;
      count.textContent = n ? `${n} selected` : "";
    }

    if (selectAll) {
      selectAll.addEventListener("change", () => {
        boxes().forEach((box) => { box.checked = selectAll.checked; });
        updateCount();
      });
    }

    document.addEventListener("change", (event) => {
      if (event.target.name !== "ids") return;
      // Unchecking any one row means "all" is no longer accurate; checking
      // every row back by hand is the only way this checkbox lights up
      // again, same as any other tri-state-avoiding "select all".
      if (!event.target.checked && selectAll) selectAll.checked = false;
      updateCount();
    });
  }

  document.addEventListener("DOMContentLoaded", initBulkSelection);

  // A bulk action button's own data-confirm carries its prompt — set in the
  // template per action (see libraryBulkAction.Confirm) rather than fixed
  // here, since only some of the actions (Done, Dismiss) are worth pausing
  // for. event.submitter is the specific button that triggered the submit,
  // which a form-level listener needs to tell "Queue it" from "Dismiss".
  document.addEventListener("submit", (event) => {
    const submitter = event.submitter;
    if (submitter && submitter.dataset.confirm && !confirm(submitter.dataset.confirm)) {
      event.preventDefault();
    }
  });

  // ---- Scroll anchoring across an extract --------------------------------
  //
  // Making an extract has htmx swap #article wholesale (see reader.html):
  // outerHTML, the whole subtree replaced with a freshly rendered one. Even
  // once the server has put width/height on every image it could measure,
  // two things still move the page under the reader when that happens: an
  // image it could not measure (SVG, AVIF), and the MathJax re-typeset just
  // below, which turns raw \(...\) source into rendered SVG asynchronously.
  // The fix is to anchor on content, not on a raw scrollY: remember which
  // block sat at the top of the viewport and exactly where, then after the
  // swap find that same block in the new HTML and scroll until it is back
  // where it was.

  // The block/position recorded just before a swap that targets #article,
  // consumed and cleared by the very next htmx:afterSwap. null whenever the
  // swap in flight is not one of these (or nothing was visible to anchor
  // to), which is what keeps an unrelated swap — the star button, a tag,
  // grading — from restoring a stale anchor left over from earlier.
  let articleAnchor = null;

  document.body.addEventListener("htmx:beforeSwap", (event) => {
    const target = event.detail && event.detail.target;
    if (!target || target.id !== "article") return;

    const block = topVisibleBlock();
    const el = document.querySelector(`#article [data-b="${block}"]`);
    articleAnchor = el ? { block, top: el.getBoundingClientRect().top } : null;
  });

  document.body.addEventListener("htmx:afterSwap", () => {
    const article = document.getElementById("article");
    if (!article) return;

    // Every htmx swap on the page bubbles this event to <body>, not just an
    // extract's — the star button, tags, deleting an extract, and grading
    // all fire it too. Retypesetting on those is a harmless no-op (already-
    // rendered TeX stays as it is), and simpler than trying to scope this
    // call to only the swaps that touch #article, so — as before this moved
    // here from an inline script in reader.html — it just always runs
    // whenever #article exists. This is the one case MathJax's own
    // automatic on-load typesetting never sees, since no page load happens.
    const typeset = window.MathJax && window.MathJax.typesetPromise
      ? window.MathJax.typesetPromise([article]).catch(() => {})
      : Promise.resolve();

    const anchor = articleAnchor;
    articleAnchor = null;
    if (!anchor) return;

    const correct = pinScroll(() => {
      const el = document.querySelector(`#article [data-b="${anchor.block}"]`);
      if (!el) return;
      const delta = el.getBoundingClientRect().top - anchor.top;
      if (Math.abs(delta) >= 1) window.scrollBy(0, delta);
    }, 2500);

    // The poll inside pinScroll would eventually catch both of these too,
    // but MathJax and a slow image both resolve on their own schedule, and
    // hooking them directly lands the correction the instant each actually
    // finishes rather than up to 120ms late.
    typeset.then(correct);
    for (const img of article.querySelectorAll("img")) {
      if (!img.complete) {
        img.addEventListener("load", correct, { once: true });
        img.addEventListener("error", correct, { once: true });
      }
    }
  });

  // ---- Theme toggle ---------------------------------------------------
  //
  // Three states cycled in one button: device (the plain CSS media query,
  // no override stored at all), light, dark. The inline script in
  // layout.html's <head> already applied whatever is in localStorage before
  // this ran, so init() here only has to make the button's own label agree
  // with reality — it must never itself decide the theme, or a page loaded
  // before this script finishes would flash the wrong one.
  const THEME_CYCLE = [null, "light", "dark"];

  function storedTheme() {
    const theme = localStorage.getItem("theme");
    return theme === "light" || theme === "dark" ? theme : null;
  }

  function themeLabel(theme) {
    if (theme === "light") return "☀ light";
    if (theme === "dark") return "☾ dark";
    return "◐ device";
  }

  function setTheme(theme) {
    if (theme) {
      document.documentElement.dataset.theme = theme;
      localStorage.setItem("theme", theme);
    } else {
      delete document.documentElement.dataset.theme;
      localStorage.removeItem("theme");
    }
    const button = document.getElementById("theme-toggle");
    if (button) button.textContent = themeLabel(theme);
  }

  const themeToggle = document.getElementById("theme-toggle");
  if (themeToggle) {
    setTheme(storedTheme());
    themeToggle.addEventListener("click", () => {
      const next = THEME_CYCLE[(THEME_CYCLE.indexOf(storedTheme()) + 1) % THEME_CYCLE.length];
      setTheme(next);
    });
  }
})();
