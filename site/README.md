# cuento documentation site

The published product/documentation site for cuento, built with plain Jekyll.
It is **self-contained**: it ships its own layouts (`_layouts/`), a shared head
and header/footer (`_includes/`), and a single hand-written stylesheet
(`assets/css/style.scss`). There is no remote theme, so the site builds with no
network access — the same boring-frontend ethos as the app it documents.

## Structure

- `index.md` — the landing page (`layout: home`). Hero, the four guiding
  constraints as a feature grid, the "what it is (and is not)" story, and a
  documentation index. The constraints and hero copy live in the page's front
  matter; the docs list lives in `_config.yml` under `docs:`.
- `architecture.md`, `security.md`, `data-integrity.md`, `features.md`,
  `rules.md` — the documentation pages (`layout: default`), a clean two-column
  doc layout with a sticky page list.
- `assets/css/style.scss` — the whole theme (design tokens, hero, cards,
  footer, doc typography). Blue primary + gold accent, system fonts only.

## Local preview

The site builds on plain Jekyll (no `github-pages` gem required):

```
bundle exec jekyll serve        # if the Gemfile bundle is installed
# or, with a standalone Jekyll:
jekyll serve
```

## Deploying on GitHub Pages

GitHub's branch-based Pages deployment can serve only the repository root or the
`/docs` folder, and both are occupied here (the root is the application, and
`docs/` holds internal working documents that must not be published). Serve this
`site/` folder with a GitHub Actions workflow instead:

1. Add a workflow that builds this directory with `actions/jekyll-build-pages`
   using `source: ./site`, uploads the result with
   `actions/upload-pages-artifact`, and deploys it with `actions/deploy-pages`.
2. In the repository settings, under **Pages** (Build and deployment), set the
   source to **GitHub Actions**.

The `repo_url` in `_config.yml` (shown in the header and footer) is a
placeholder — replace it with the real repository URL before publishing.
