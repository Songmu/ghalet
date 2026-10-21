# Documentation site

[日本語](README.ja.md)

The site follows the Hugo/Hextra setup in Songmu/tagpr. It renders the repository
README as the home page and the Markdown pages under `docs/` as guides.
Edit those source files in both languages; no separate site content is maintained.
The build adds frontmatter, checks translation pairs, and rewrites Markdown links
for the site. Links to examples and other repository files stay on GitHub.

## Local build

Install the extended Hugo version in `.hugo-version` and Go as specified in
`go.mod`, then run from the repository root:

```sh
bash docs/site/build.sh
```

The generated site is written to the ignored `site/` directory. For a fresh
build without stale output, use an empty destination:

```sh
bash docs/site/build.sh "$(mktemp -d)"
```

To build and preview at a local base path:

```sh
HUGO_BASEURL=http://localhost:8000/site/ bash docs/site/build.sh
python3 -m http.server 8000 --directory .
```

Open `http://localhost:8000/site/` for English or
`http://localhost:8000/site/ja/` for Japanese.

## GitHub Pages

In repository Settings, select **Pages → Build and deployment → Source →
GitHub Actions** once before the first deployment.

The `pages` workflow builds pull requests that change `docs/`, either README,
or the workflow itself. It deploys only pushes to `main` affecting those paths,
or a manual run on `main`. No documentation tags or tagpr workflow are needed.
The deployment uses the base URL returned by GitHub Pages, including any
configured custom domain. Pull request and local builds default to
`https://songmu.github.io/ghalet/`.
