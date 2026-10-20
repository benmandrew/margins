// Markup for typdiff --no-preamble output. The service writes this file to
// /.thesis-diff/diff-markup.typ in the scratch tree and imports it at the top
// of every file it rewrites.
#let diff-added(body) = {
  set text(fill: rgb("#1a5fb4"))
  underline(body)
}
#let diff-deleted(body) = {
  set text(fill: rgb("#c01c28"))
  strike(body)
}
#let diff-margin-mark(color, note) = place(left, dx: -1.9cm, box(width: 1.7cm, {
  set text(size: 7pt, fill: color, weight: "regular", style: "normal")
  set par(justify: false, first-line-indent: 0pt)
  box(fill: color, width: 2pt, height: 0.9em)
  h(2pt)
  note
}))
#let diff-added-mark(note: "added") = diff-margin-mark(rgb("#1a5fb4"), note)
#let diff-deleted-mark(note: "deleted") = diff-margin-mark(rgb("#c01c28"), note)
#let diff-changed-mark(note: "changed") = diff-margin-mark(rgb("#e07000"), note)
