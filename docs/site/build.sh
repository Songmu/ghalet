#!/usr/bin/env bash
set -euo pipefail

site_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_dir="$(cd "$site_dir/.." && pwd)"
repo_dir="$(cd "$docs_dir/.." && pwd)"
output_dir="${1:-"$repo_dir/site"}"
if [[ "$output_dir" != /* ]]; then
  output_dir="$PWD/$output_dir"
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

mkdir -p "$work_dir/content" "$work_dir/static"
cp "$site_dir/hugo.yaml" "$site_dir/go.mod" "$site_dir/go.sum" "$work_dir/"
cp -R "$site_dir/layouts" "$work_dir/layouts"
cp -R "$docs_dir/images" "$work_dir/static/images"

add_page() {
  local source="$1"
  local relative="$2"
  local weight="$3"
  local counterpart title target
  if [[ "$source" == *.ja.md ]]; then
    counterpart="${source%.ja.md}.md"
  else
    counterpart="${source%.md}.ja.md"
  fi
  if [[ ! -f "$counterpart" ]]; then
    echo "missing documentation translation for: $source" >&2
    exit 1
  fi
  IFS= read -r title < "$source"
  if [[ "$title" != "# "* ]]; then
    echo "documentation page must start with a level-one heading: $source" >&2
    exit 1
  fi
  title="${title#\# }"
  title="${title//\\/\\\\}"
  title="${title//\"/\\\"}"
  target="$work_dir/content/$relative"
  mkdir -p "$(dirname "$target")"
  {
    printf '%s\n' '---'
    printf 'title: "%s"\nweight: %s\n' "$title" "$weight"
    if [[ "$relative" == _index* ]]; then
      printf '%s\n' 'type: docs' 'cascade:' '  type: docs'
    fi
    printf '%s\n\n' '---'
    tail -n +2 "$source"
  } > "$target"
}

add_page "$docs_dir/index.md" _index.md 1
add_page "$docs_dir/index.ja.md" _index.ja.md 1

while IFS= read -r -d '' source; do
  relative="${source#"$docs_dir/"}"
  logical="${relative%.ja.md}"
  logical="${logical%.md}"
  case "$logical" in
    index) continue ;;
    getting-started) weight=10 ;;
    distribution) weight=20 ;;
    authentication) weight=30 ;;
    app-setup) weight=40 ;;
    file-credentials) weight=50 ;;
    credentials) weight=60 ;;
    operations) weight=70 ;;
    *) weight=100 ;;
  esac
  add_page "$source" "$relative" "$weight"
done < <(
  find "$docs_dir" \
    -path "$site_dir" -prune -o \
    -type f -name '*.md' ! -name AGENTS.md -print0
)

hugo \
  --source "$work_dir" \
  --destination "$output_dir" \
  --gc \
  --minify \
  --panicOnWarning \
  --printPathWarnings
