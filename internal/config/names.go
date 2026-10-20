package config

import "regexp"

// BuildFile is the per-commit build configuration in the thesis repository.
const BuildFile = ".typst-review.toml"

// Strict patterns for every name that reaches a path or a git argument. None
// of them can start with '-', so no value can be read as a git option, and
// none admits a '..' path segment.
var (
	TypstVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$`)
	SHA          = regexp.MustCompile(`^[0-9a-f]{40}$`)
	ShortSHA     = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	// Branch and tag names: a conservative subset of git-check-ref-format.
	BranchName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]{0,99}$`)
	RelPath    = regexp.MustCompile(`^(?:[A-Za-z0-9_][A-Za-z0-9._-]*)(?:/[A-Za-z0-9_][A-Za-z0-9._-]*)*$`)
	Entrypoint = regexp.MustCompile(`^(?:[A-Za-z0-9_][A-Za-z0-9._-]*/)*[A-Za-z0-9_][A-Za-z0-9._-]*\.typ$`)

	UserID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	NoteID = regexp.MustCompile(`^n[0-9]+$`)

	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	badRef    = regexp.MustCompile(`\.\.|//|/\.|\.lock$|/$|\.$`)
)

// ValidRef reports whether a branch or tag name is safe to use. The pattern
// alone admits "a/../b" and "a..b", which git rejects and a path join would
// resolve, so those are refused here too.
func ValidRef(name string) bool {
	if !BranchName.MatchString(name) {
		return false
	}
	return !badRef.MatchString(name)
}
