#!/usr/bin/env bash
# Builds the fixture thesis repository the acceptance tests run against.
#
#   scripts/make-fixture.sh <dir>
#
# Every author and committer date is fixed, so the commit SHAs are the same on
# every run. The history:
#
#   c1  initial thesis: main.typ including two chapters, Typst 0.15.1   review-1
#   c2  pins Typst 0.14.2, small prose edit
#   c3  back to 0.15.1; edits prose, changes an equation and the table, adds a chapter
#   c4  does not compile
#   c5  fixes it                                                         review-2
set -euo pipefail

dir=${1:?usage: make-fixture.sh <dir>}
mkdir -p "$dir"
cd "$dir"

export GIT_CONFIG_NOSYSTEM=1 HOME="$PWD/.home"
export GIT_AUTHOR_NAME="Ben" GIT_AUTHOR_EMAIL="ben@example.com"
export GIT_COMMITTER_NAME="Ben" GIT_COMMITTER_EMAIL="ben@example.com"
mkdir -p "$HOME"
git init --quiet --initial-branch=main .
git config commit.gpgsign false
git config tag.gpgsign false
echo ".home/" > .git/info/exclude

commit() {
  local date=$1 msg=$2
  git add -A
  GIT_AUTHOR_DATE="$date" GIT_COMMITTER_DATE="$date" git commit --quiet -m "$msg"
}

tag() {
  local date=$1 name=$2
  GIT_COMMITTER_DATE="$date" git tag -a "$name" -m "$name"
}

cat > .typst-review.toml <<'EOF'
typst = "0.15.1"
entrypoints = ["main.typ"]
branches = ["main"]
review_tags = "review-*"
EOF

cat > main.typ <<'EOF'
#set document(title: "Fixture thesis")
#set heading(numbering: "1.")
#set math.equation(numbering: "(1)")

= Fixture thesis

#include "ch1.typ"
#include "ch2.typ"

#bibliography("refs.bib")
EOF

cat > refs.bib <<'EOF'
@article{knuth,
  title = {Literate Programming},
  author = {Knuth, Donald E.},
  journal = {The Computer Journal},
  year = {1984},
}
@article{dijkstra,
  title = {Go To Statement Considered Harmful},
  author = {Dijkstra, Edsger W.},
  journal = {Communications of the ACM},
  year = {1968},
}
EOF

cat > ch1.typ <<'EOF'
= Introduction <intro>

Programs are written for people to read @knuth. The energy is $E = m c^2$ in
this simple model.

$ integral_0^1 x^2 dif x = 1/3 $ <eq-int>

Equation @eq-int and @fig-table hold the results.

#figure(
  table(columns: 2, [Input], [Output], [1], [2]),
  caption: [A small table],
) <fig-table>

```python
print("raw blocks are not markup")
```

#let samples = 3
We took #samples samples.
EOF

cat > ch2.typ <<'EOF'
= Method <method>

We measure everything twice, as @intro explains.
EOF

commit "2026-01-01T10:00:00Z" "Initial draft"
tag "2026-01-02T10:00:00Z" review-1

sed -i 's/typst = "0.15.1"/typst = "0.14.2"/' .typst-review.toml
sed -i 's/We measure everything twice/We measure everything carefully twice/' ch2.typ
commit "2026-01-03T10:00:00Z" "Try the older Typst"

sed -i 's/typst = "0.14.2"/typst = "0.15.1"/' .typst-review.toml
sed -i 's/Programs are written for people to read @knuth./Programs are written mainly for people to read @knuth @dijkstra./' ch1.typ
sed -i 's/x^2 dif x = 1\/3/x^3 dif x = 1\/4/' ch1.typ
sed -i 's/\[1\], \[2\]/[1], [3]/; s/A small table/A slightly larger table/' ch1.typ
sed -i 's/#include "ch2.typ"/#include "ch2.typ"\n#include "ch3.typ"/' main.typ
cat > ch3.typ <<'EOF'
= Results

The results in @method are encouraging.
EOF
commit "2026-01-04T10:00:00Z" "Rework the introduction and add results"

echo '#this-function-does-not-exist()' >> ch2.typ
commit "2026-01-05T10:00:00Z" "Break the build"

sed -i '/this-function-does-not-exist/d' ch2.typ
commit "2026-01-06T10:00:00Z" "Fix the build"
tag "2026-01-07T10:00:00Z" review-2
