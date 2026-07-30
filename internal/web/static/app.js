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

  // A cloze is addressed against the extract's stored text, which is a single
  // run of characters rather than a set of blocks. Because an extract's own
  // text starts at block zero, the offsets within it are what the server wants.
  window.clozePayload = () => {
    if (!captured) return {};
    return { start: captured.start_offset, end: captured.end_offset };
  };

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

    const box = range.getBoundingClientRect();
    bar.hidden = false;
    bar.style.top = `${box.top + window.scrollY - bar.offsetHeight - 8}px`;
    bar.style.left = `${box.left + window.scrollX}px`;
  }

  document.addEventListener("selectionchange", () => {
    const payload = readSelection();
    const bar = toolbar();
    if (!bar) return;

    if (!payload) {
      // Do not hide while a toolbar button is being clicked: the click itself
      // collapses the selection, and hiding here would cancel it.
      if (!bar.contains(document.activeElement)) {
        bar.hidden = true;
        captured = null;
      }
      return;
    }

    captured = payload;
    showToolbar(window.getSelection().getRangeAt(0));
  });

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

  function trackReadingPosition() {
    const reader = document.querySelector(".reader");
    if (!reader) return;

    // Resume where reading stopped. Doing this before wiring up the scroll
    // listener avoids recording the jump itself as progress.
    const resumeAt = parseInt(reader.dataset.readBlock, 10);
    if (resumeAt > 0) {
      const target = document.querySelector(`#article [data-b="${resumeAt}"]`);
      if (target) target.scrollIntoView();
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
