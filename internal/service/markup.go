package service

import (
	"unicode/utf8"
)

// Position classes from scanMarkup, one per byte offset of a source file.
const (
	posOther  = iota // code, a string, math, raw text, a comment or a label
	posMarkup        // inside markup, but not a place for an inline marker
	posInline        // plain markup text: an inline #review-note(...) is safe
)

// scanMarkup classifies every byte offset of a Typst file by whether a
// `#review-note("nN");` inserted there lands in plain markup. It is a
// lightweight lexer, not a parser: it follows markup, code, strings, maths,
// raw text and comments well enough to keep markers out of the places where
// they would fail to compile or change the output. When it is wrong the
// compile fails and the annotated build falls back a level.
func scanMarkup(src []byte) []uint8 {
	sc := &scanner{src: src, class: make([]uint8, len(src)+1)}
	sc.markup(0, 0, true)
	return sc.class
}

type scanner struct {
	src   []byte
	class []uint8
}

func (sc *scanner) at(i int) byte {
	if i < len(sc.src) {
		return sc.src[i]
	}
	return 0
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9' || c == '-'
}

// lineStart reports whether only spaces or tabs precede i on its line.
func (sc *scanner) lineStart(i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch sc.src[j] {
		case ' ', '\t':
			continue
		case '\n':
			return true
		default:
			return false
		}
	}
	return true
}

// markup scans markup from i until the byte close (0 for end of file) at
// bracket depth zero, and returns the offset after close. inline is false
// inside content that a binding or a rule holds, where a marker would show
// up wherever that content is used.
func (sc *scanner) markup(i int, close byte, inline bool) int {
	depth := 0
	level := uint8(posMarkup)
	if inline {
		level = posInline
	}
	for i < len(sc.src) {
		c := sc.src[i]
		if c == close && depth == 0 {
			return i + 1
		}
		if i == 0 || sc.src[i-1] == '\n' {
			// A whole line inserted here stays in markup, whatever the
			// line starts with.
			sc.class[i] = posMarkup
		}
		if sc.lineStart(i) && c != ' ' && c != '\t' && c != '\n' {
			// A heading is never a place for a marker: it would show in the
			// outline. A list or term marker must stay first on its line.
			if c == '=' {
				i = sc.skipLine(i)
				continue
			}
			if (c == '-' || c == '+' || c == '/') && (sc.at(i+1) == ' ' || sc.at(i+1) == '\t') {
				sc.class[i] = posMarkup
				i += 2
				continue
			}
			if c >= '0' && c <= '9' {
				j := i
				for j < len(sc.src) && sc.src[j] >= '0' && sc.src[j] <= '9' {
					sc.class[j] = posMarkup
					j++
				}
				if sc.at(j) == '.' {
					sc.class[j] = posMarkup
					i = j + 1
					continue
				}
			}
		}
		switch {
		case c == '\\':
			sc.class[i] = level
			i += 2 // the escaped character is part of the escape
			continue
		case c == '/' && sc.at(i+1) == '/' && !sc.afterScheme(i):
			i = sc.lineComment(i)
			continue
		case c == '/' && sc.at(i+1) == '*':
			i = sc.blockComment(i)
			continue
		case c == '`':
			i = sc.raw(i)
			continue
		case c == '$':
			i = sc.math(i + 1)
			continue
		case c == '#':
			i = sc.embedded(i + 1)
			continue
		case c == '<' && isIdentStart(sc.at(i+1)):
			if j := sc.label(i); j > i {
				i = j
				continue
			}
		case c == '@' && isIdentStart(sc.at(i+1)):
			// A reference, possibly with a [supplement] after it.
			j := i + 1
			for j < len(sc.src) && (isIdentChar(sc.src[j]) || sc.src[j] == ':' || sc.src[j] == '.' && isIdentChar(sc.at(j+1))) {
				j++
			}
			i = j
			if sc.at(i) == '[' {
				i = sc.markup(i+1, ']', inline)
			}
			continue
		case c == '[':
			depth++
		case c == ']':
			depth--
		case c == 'h' && sc.startsWith(i, "http://"), c == 'h' && sc.startsWith(i, "https://"):
			// A bare link runs to the next space; a marker inside it would
			// break the URL.
			for i < len(sc.src) && sc.src[i] != ' ' && sc.src[i] != '\n' && sc.src[i] != '\t' {
				i++
			}
			continue
		}
		if utf8.RuneStart(c) {
			sc.class[i] = level
		}
		i++
	}
	if i == len(sc.src) && close == 0 {
		sc.class[i] = level
	}
	return i
}

func (sc *scanner) startsWith(i int, s string) bool {
	return len(sc.src)-i >= len(s) && string(sc.src[i:i+len(s)]) == s
}

func (sc *scanner) afterScheme(i int) bool {
	return i > 0 && sc.src[i-1] == ':'
}

func (sc *scanner) skipLine(i int) int {
	for i < len(sc.src) && sc.src[i] != '\n' {
		i++
	}
	return i
}

func (sc *scanner) lineComment(i int) int { return sc.skipLine(i) }

func (sc *scanner) blockComment(i int) int {
	depth := 0
	for i < len(sc.src) {
		switch {
		case sc.src[i] == '/' && sc.at(i+1) == '*':
			depth++
			i += 2
		case sc.src[i] == '*' && sc.at(i+1) == '/':
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return i
}

// raw skips inline raw text or a raw block that starts at i.
func (sc *scanner) raw(i int) int {
	n := 0
	for sc.at(i+n) == '`' {
		n++
	}
	if n == 2 {
		return i + 2 // empty raw text
	}
	j := i + n
	for j < len(sc.src) {
		if sc.src[j] != '`' {
			j++
			continue
		}
		m := 0
		for sc.at(j+m) == '`' {
			m++
		}
		if m >= n {
			return j + n
		}
		j += m
	}
	return j
}

// math skips an equation; i is just after the opening $.
func (sc *scanner) math(i int) int {
	for i < len(sc.src) {
		switch sc.src[i] {
		case '\\':
			i += 2
			continue
		case '$':
			return i + 1
		case '"':
			i = sc.str(i + 1)
			continue
		case '#':
			i = sc.embedded(i + 1)
			continue
		case '/':
			if sc.at(i+1) == '/' {
				i = sc.lineComment(i)
				continue
			}
		}
		i++
	}
	return i
}

// str skips a string; i is just after the opening quote.
func (sc *scanner) str(i int) int {
	for i < len(sc.src) {
		switch sc.src[i] {
		case '\\':
			i += 2
			continue
		case '"':
			return i + 1
		}
		i++
	}
	return i
}

func (sc *scanner) label(i int) int {
	j := i + 1
	for j < len(sc.src) && (isIdentChar(sc.src[j]) || sc.src[j] == ':' || sc.src[j] == '.') {
		j++
	}
	if sc.at(j) == '>' {
		return j + 1
	}
	return i
}

var statementKeywords = map[string]bool{
	"let": true, "set": true, "show": true, "import": true, "include": true,
	"if": true, "for": true, "while": true, "context": true, "return": true,
	"break": true, "continue": true,
}

// holdsContent are statements whose content blocks are stored or reused, not
// placed where they appear.
var holdsContent = map[string]bool{"let": true, "set": true, "show": true}

// embedded skips an embedded code expression; i is just after the #.
func (sc *scanner) embedded(i int) int {
	c := sc.at(i)
	switch {
	case isIdentStart(c):
		j := i
		for j < len(sc.src) && isIdentChar(sc.src[j]) {
			j++
		}
		word := string(sc.src[i:j])
		if statementKeywords[word] {
			j = sc.code(j, '\n', !holdsContent[word])
			if j > 0 && sc.at(j-1) == '\n' {
				j-- // the newline belongs to the markup
			}
			return sc.semicolon(j)
		}
		return sc.postfix(j)
	case c == '(':
		return sc.postfix(sc.code(i+1, ')', true))
	case c == '{':
		return sc.postfix(sc.code(i+1, '}', true))
	case c == '[':
		return sc.postfix(sc.markup(i+1, ']', true))
	case c == '"':
		return sc.postfix(sc.str(i + 1))
	}
	return i
}

// postfix skips calls, trailing content blocks and field accesses after a
// primary expression.
func (sc *scanner) postfix(i int) int {
	for {
		switch c := sc.at(i); {
		case c == '(':
			i = sc.code(i+1, ')', true)
		case c == '[':
			i = sc.markup(i+1, ']', true)
		case c == '.' && isIdentStart(sc.at(i+1)):
			i++
			for i < len(sc.src) && isIdentChar(sc.src[i]) {
				i++
			}
		default:
			return sc.semicolon(i)
		}
	}
}

func (sc *scanner) semicolon(i int) int {
	if sc.at(i) == ';' {
		return i + 1
	}
	return i
}

// code skips code until close at depth zero and returns the offset after
// it. Content blocks inside are markup again.
func (sc *scanner) code(i int, close byte, inline bool) int {
	for i < len(sc.src) {
		c := sc.src[i]
		switch {
		case c == close:
			return i + 1
		case c == '(':
			i = sc.code(i+1, ')', inline)
		case c == '{':
			i = sc.code(i+1, '}', inline)
		case c == '[':
			i = sc.markup(i+1, ']', inline)
		case c == '"':
			i = sc.str(i + 1)
		case c == '$':
			i = sc.math(i + 1)
		case c == '`':
			i = sc.raw(i)
		case c == '/' && sc.at(i+1) == '/':
			i = sc.lineComment(i)
		case c == '/' && sc.at(i+1) == '*':
			i = sc.blockComment(i)
		default:
			i++
		}
	}
	return i
}
