// Turns a text selection in the source table into the anchor the server
// expects: a 1-based line, a 1-based UTF-8 byte column, and the selected
// text. The server checks the text is at that position before saving.
"use strict";

(function () {
  const table = document.getElementById("source");
  const form = document.getElementById("note-form");
  if (!table || !form) return;
  const line = document.getElementById("f-line");
  const col = document.getElementById("f-col");
  const selected = document.getElementById("f-selected");
  const hint = document.getElementById("selection");
  const submit = document.getElementById("note-submit");
  const enc = new TextEncoder();

  function cellOf(node) {
    const el = node.nodeType === Node.ELEMENT_NODE ? node : node.parentElement;
    return el && el.closest("td.code");
  }

  // Text of a code cell before (node, offset).
  function prefix(cell, node, offset) {
    const r = document.createRange();
    r.selectNodeContents(cell);
    r.setEnd(node, offset);
    return r.toString();
  }

  function lineText(n) {
    const cell = table.querySelector('td.code[data-line="' + n + '"]');
    return cell ? cell.textContent : "";
  }

  function update() {
    const sel = window.getSelection();
    if (!sel || sel.rangeCount === 0 || sel.isCollapsed) return;
    const r = sel.getRangeAt(0);
    const startCell = cellOf(r.startContainer);
    const endCell = cellOf(r.endContainer);
    if (!startCell || !endCell || !table.contains(startCell)) return;
    const startLine = Number(startCell.dataset.line);
    const endLine = Number(endCell.dataset.line);
    const before = prefix(startCell, r.startContainer, r.startOffset);
    // Rebuild the selection from the cells, so line numbers and margin
    // notes inside the range never leak into it.
    let text;
    if (startLine === endLine) {
      text = prefix(startCell, r.endContainer, r.endOffset).slice(before.length);
    } else {
      const parts = [lineText(startLine).slice(before.length)];
      for (let n = startLine + 1; n < endLine; n++) parts.push(lineText(n));
      parts.push(prefix(endCell, r.endContainer, r.endOffset));
      text = parts.join("\n");
    }
    if (!text) return;
    line.value = String(startLine);
    col.value = String(enc.encode(before).length + 1);
    selected.value = text;
    const shown = text.length > 80 ? text.slice(0, 77) + "…" : text;
    hint.textContent = "Line " + startLine + ": “" + shown + "”";
    submit.disabled = false;
  }

  document.addEventListener("selectionchange", update);
})();
