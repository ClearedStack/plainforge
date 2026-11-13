#!/bin/sh
# Rebuilds the saved packs in internal/pack/testdata with an ordinary Git.
# Everything in them is made up. The tests do not run this script; they read
# the saved files. Object names stay the same on every run because the
# content, the names and the dates are fixed here. The compressed bytes of a
# pack can differ between Git versions, which is why the packs are saved.
set -eu
out="$(cd "$(dirname "$0")/.." && pwd)/internal/pack/testdata"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME="Example Author" GIT_AUTHOR_EMAIL="author@example.invalid"
export GIT_COMMITTER_NAME="Example Author" GIT_COMMITTER_EMAIL="author@example.invalid"
export TZ=UTC
cd "$work"
git init -q -b main .

lines() { i=1; while [ "$i" -le "$1" ]; do echo "widget line $i: lorem ipsum dolor sit amet $2"; i=$((i+1)); done; }

stamp() { export GIT_AUTHOR_DATE="2001-01-0$1T00:00:00Z" GIT_COMMITTER_DATE="2001-01-0$1T00:00:00Z"; }

stamp 1
mkdir src
echo "Example widget notes" > README.txt
lines 200 one > src/widget.txt
git add -A && git commit -q -m "Add the example widget"
A=$(git rev-parse HEAD)

stamp 2
lines 200 one | sed 's/line 77:/line 77 (changed):/' > src/widget.txt
mkdir docs && echo "How to hold the example widget" > docs/guide.txt
printf 'name: example\nimage: example@sha256:%064d\n' 1 > app.yaml
git add -A && git commit -q -m "Describe the example widget"
B=$(git rev-parse HEAD)
git tag -a v0.0.1 -m "First made-up release"
T=$(git rev-parse v0.0.1)

stamp 3
lines 200 one | sed 's/line 77:/line 77 (changed):/; s/line 150:/line 150 (changed):/' > src/widget.txt
printf 'name: example\nimage: example@sha256:%064d\n' 2 > app.yaml
git add -A && git commit -q -m "Update the example image digest [skip-ci]"
C=$(git rev-parse HEAD)

stamp 4
git checkout -q -b side "$A"
echo "A side note" > side.txt
git add -A && git commit -q -m "Add a side note"
S=$(git rev-parse HEAD)

echo "$B"            | git pack-objects -q --stdout --revs --delta-base-offset > "$out/first-ofs.pack"
echo "$B"            | git pack-objects -q --stdout --revs                     > "$out/first-ref.pack"
printf '%s\n^%s\n' "$C" "$B" | git pack-objects -q --stdout --revs --thin      > "$out/second-thin.pack"
printf '%s\n^%s\n' "$T" "$B" | git pack-objects -q --stdout --revs             > "$out/tag.pack"
printf '%s\n^%s\n' "$S" "$A" | git pack-objects -q --stdout --revs             > "$out/side.pack"

list() { git rev-list --objects --no-object-names "$@" | git cat-file --batch-check='%(objectname) %(objecttype)' | sort; }
{
  echo "A $A"; echo "B $B"; echo "C $C"; echo "S $S"; echo "T $T"
} > "$out/names.txt"
list "$B" > "$out/first.objects"
list "$C" "^$B" > "$out/second.objects"
list "$T" "^$B" > "$out/tag.objects"
list "$S" "^$A" > "$out/side.objects"
for p in first-ofs first-ref second-thin; do
  git index-pack --fix-thin --stdin < "$out/$p.pack" >/dev/null
done
for i in .git/objects/pack/*.idx; do git verify-pack -v "$i" | grep -c ' [0-9a-f]\{40\}$' || true; done
