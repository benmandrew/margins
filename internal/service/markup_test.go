package service

import (
	"strings"
	"testing"

	"git.benmandrew.com/me/thesis/internal/gitx"
)

func TestScanMarkup(t *testing.T) {
	src := strings.Join([]string{
		`= Heading <h>`,                                // 1
		`Plain text with $x^2$ and @ref.`,              // 2
		`#figure(table([cell]), caption: [Cap]) <lbl>`, // 3
		"```",                            // 4
		`raw`,                            // 5
		"```",                            // 6
		`#let x = [stored]`,              // 7
		`- item // comment`,              // 8
		`See https://example.com/a now.`, // 9
		`Call #f("str")[body] after.`,    // 10
		`/* block */ text \# esc`,        // 11
	}, "\n")
	class := scanMarkup([]byte(src))
	at := func(line int, needle string, delta int) uint8 {
		t.Helper()
		lines := strings.Split(src, "\n")
		off := 0
		for i := 0; i < line-1; i++ {
			off += len(lines[i]) + 1
		}
		i := strings.Index(lines[line-1], needle)
		if i < 0 {
			t.Fatalf("line %d has no %q", line, needle)
		}
		return class[off+i+delta]
	}
	for _, tt := range []struct {
		line   int
		needle string
		delta  int
		want   uint8
	}{
		{1, "Heading", 0, posOther},
		{2, "Plain", 0, posInline},
		{2, "x^2", 0, posOther},
		{2, " and", 0, posInline},
		{2, "ref", 0, posOther},
		{3, "cell", 0, posInline},
		{3, "Cap", 0, posInline},
		{3, "table", 0, posOther},
		{3, "lbl", 0, posOther},
		{5, "raw", 0, posOther},
		{7, "stored", 0, posMarkup},
		{8, "- item", 0, posMarkup},
		{8, "item", 0, posInline},
		{8, "comment", 0, posOther},
		{9, "example", 0, posOther},
		{9, " now", 0, posInline},
		{10, "str", 0, posOther},
		{10, "body", 0, posInline},
		{10, " after", 0, posInline},
		{11, "block", 0, posOther},
		{11, "text", 0, posInline},
		{11, "#", 0, posOther},
	} {
		if got := at(tt.line, tt.needle, tt.delta); got != tt.want {
			t.Errorf("line %d at %q: class %d, want %d", tt.line, tt.needle, got, tt.want)
		}
	}
}

func TestParagraphStart(t *testing.T) {
	src := []byte("a\n\nb c\nd e\n\nf")
	off := strings.Index(string(src), "e")
	if got := paragraphStart(src, off); got != 3 {
		t.Errorf("paragraphStart = %d, want 3", got)
	}
	if got := paragraphStart(src, 0); got != 0 {
		t.Errorf("paragraphStart at file start = %d", got)
	}
}

func TestMapLine(t *testing.T) {
	hunks := []gitx.Hunk{
		{OldStart: 2, OldLines: 0, NewStart: 3, NewLines: 2},  // two lines inserted after line 2
		{OldStart: 5, OldLines: 1, NewStart: 7, NewLines: 1},  // line 5 changed
		{OldStart: 8, OldLines: 2, NewStart: 10, NewLines: 0}, // lines 8 and 9 deleted
	}
	for _, tt := range []struct {
		in, out int
		changed bool
	}{
		{1, 1, false},
		{2, 2, false},
		{3, 5, false},
		{5, 7, true},
		{7, 9, false},
		{8, 10, true},
		{10, 10, false},
	} {
		out, changed := mapLine(tt.in, hunks)
		if out != tt.out || changed != tt.changed {
			t.Errorf("mapLine(%d) = %d, %v; want %d, %v", tt.in, out, changed, tt.out, tt.changed)
		}
	}
}
