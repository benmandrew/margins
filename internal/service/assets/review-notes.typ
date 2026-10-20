// Supervisor notes for annotated builds. The service writes this file and
// notes.json to /.thesis-review/ in a scratch tree. Note text reaches Typst
// only as JSON strings, which are drawn as text and never evaluated.
#let _notes = json("/.thesis-review/notes.json")

#let _swatch(n) = rgb(n.colour)

#let _label(n) = text(size: 6.5pt, weight: "bold", fill: _swatch(n), n.number + " " + n.name)

// The bottom of the last margin note on each page, so notes that start close
// together stack instead of overlapping.
#let _bottom = state("review-note-bottom", (page: 0, y: 0pt))

#let _card(n) = block(
  width: 2.1cm,
  inset: (left: 3pt),
  stroke: (left: 1.5pt + _swatch(n)),
  {
    set par(justify: false, first-line-indent: 0pt, leading: 0.35em)
    set text(size: 6.5pt, fill: black, weight: "regular", style: "normal", hyphenate: true)
    _label(n)
    linebreak()
    n.text
  },
)

// A small numbered flag at the anchor, and the note in the right margin at the
// same height, or just below the note before it.
#let review-note(id) = {
  let n = _notes.at(id)
  box(baseline: -0.45em, text(size: 6pt, weight: "bold", fill: _swatch(n), "[" + n.number + "]"))
  box(width: 0pt, context {
    let pos = here().position()
    let card = _card(n)
    let h = measure(card).height
    let prev = _bottom.get()
    let y = pos.y - 0.8em.to-absolute()
    if prev.page == pos.page and prev.y > y { y = prev.y }
    _bottom.update((page: pos.page, y: y + h + 3pt))
    place(top + left, dx: page.width - pos.x - 2.3cm, dy: y - pos.y, card)
  })
}

// The notes that got no marker, in one list at the end of the document.
#let review-unplaced(ids) = {
  pagebreak(weak: true)
  heading(numbering: none, outlined: false, "Unplaced notes")
  for id in ids {
    let n = _notes.at(id)
    block(
      width: 100%,
      inset: (left: 6pt, y: 4pt),
      stroke: (left: 2pt + _swatch(n)),
      {
        _label(n)
        h(4pt)
        text(size: 8pt, fill: luma(90), n.file + ":" + str(n.line))
        linebreak()
        n.text
      },
    )
  }
}
