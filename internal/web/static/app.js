// increader — the only hand-written JavaScript in the project.
//
// Its whole job is translating a text selection into the coordinates the server
// addresses passages by: a block index (the data-b attribute the server put on
// every paragraph) plus a character offset into that block's text. Everything
// else is htmx.

(function () {
  "use strict";

  // The selection captured when the toolbar appeared. It has to be stored,
  // because clicking a button collapses the selection before htmx reads it.
  let captured = null;

  const toolbar = () => document.getElementById("selection-toolbar");

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
    const bar = toolbar();
    if (bar) bar.hidden = true;
    window.getSelection().removeAllRanges();
  };

  /** Shows the toolbar just above the selected text. */
  function showToolbar(range) {
    const bar = toolbar();
    if (!bar) return;

    // Unhide before measuring: offsetHeight is 0 while the element is hidden,
    // which would place the toolbar on top of the text instead of above it.
    bar.hidden = false;

    const box = range.getBoundingClientRect();
    const top = box.top + window.scrollY - bar.offsetHeight - 8;

    // Below the selection when there is no room above it, rather than off the
    // top of the page.
    bar.style.top = `${top < window.scrollY ? box.bottom + window.scrollY + 8 : top}px`;

    // Keep it on screen when the selection starts near the right edge.
    const left = Math.min(box.left + window.scrollX,
                          document.documentElement.clientWidth - bar.offsetWidth - 8);
    bar.style.left = `${Math.max(8, left)}px`;
  }

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

  // ---- Paste-to-extract fallback -----------------------------------------
  //
  // Belt and braces: the toolbar tap can still be unreliable right after a
  // selection on iOS, where the first touch outside it is sometimes consumed
  // by the OS to dismiss its own selection UI before any click reaches the
  // page. This path never depends on that tap at all. Selecting and tapping
  // "Copy" is native OS behaviour and always works, and stays plain
  // clipboard copy — nothing here hooks it. From there the user pastes into
  // an ordinary text input elsewhere on the page, which is an ordinary tap
  // with no active selection UI for iOS to intercept.

  /** Collapses whitespace runs and trims, matching the server's own
   * normalization so a pasted quote compares the way it will once it
   * reaches the extract handler. */
  function normalizeSpace(text) {
    return text.replace(/\s+/g, " ").trim();
  }

  /**
   * Finds `normalized` inside `raw`, treating any whitespace run in the
   * search text as equivalent to any whitespace run in the source. Pasted
   * text often reflows line breaks differently than the DOM's own text
   * content even though both hold the same words, so a plain indexOf would
   * miss real matches.
   */
  function findInRawText(raw, normalized) {
    if (!normalized) return null;
    const pattern = normalized
      .replace(/[.*+?^${}()|[\]\\]/g, "\\$&")
      .replace(/ /g, "\\s+");
    const match = new RegExp(pattern).exec(raw);
    if (!match) return null;
    return { start: match.index, end: match.index + match[0].length, text: match[0] };
  }

  window.extractFromPaste = () => {
    const input = document.getElementById("paste-extract-input");
    if (!input) return;

    const pasted = normalizeSpace(input.value);
    if (!pasted) return;

    const reader = document.querySelector(".reader");
    if (!reader) return;

    // Only a single block is searched: extracts spanning more than one
    // paragraph are rare enough, and locating a cross-block match's offsets
    // reliably enough to trust, that it is not worth the added complexity
    // here. Select one paragraph at a time for those.
    for (const block of document.querySelectorAll("#article [data-b]")) {
      const hit = findInRawText(block.textContent, pasted);
      if (!hit) continue;

      fetch(`/elements/${reader.dataset.element}/extract`, {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: new URLSearchParams({
          start_block: block.dataset.b,
          start_offset: String(hit.start),
          end_block: block.dataset.b,
          end_offset: String(hit.end),
          quote: hit.text,
        }),
      })
        .then((res) => (res.ok ? res.text() : Promise.reject(res)))
        .then((html) => {
          document.getElementById("article").outerHTML = html;
          input.value = "";
        })
        .catch(() => {
          alert("That text could not be extracted — it may not match the article exactly.");
        });
      return;
    }

    alert("No matching passage found. If the copied text spans more than one paragraph, try one paragraph at a time.");
  };

  // ---- Reading position -------------------------------------------------

  /** The index of the topmost block still visible in the viewport. */
  function topVisibleBlock() {
    const blocks = document.querySelectorAll("#article [data-b]");
    for (const block of blocks) {
      if (block.getBoundingClientRect().bottom > 0) {
        return parseInt(block.dataset.b, 10);
      }
    }
    return 0;
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
      if (target) target.scrollIntoView({ block: "start" });
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
})();
