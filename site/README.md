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
`docs/` holds internal working documents that must not be published). So the
`site/` folder is built and published by a GitHub Actions workflow.

The ready-to-use workflow is kept at [`deploy/pages-workflow.yml`](../deploy/pages-workflow.yml).
It is stored there — not under `.github/workflows/` — because a Personal Access
Token without the `workflow` scope is refused when a push touches
`.github/workflows/`. Add it through the GitHub web UI (which is allowed to
commit workflow files) instead:

1. On github.com: repo → **Actions** tab → **New workflow** → **set up a workflow
   yourself**. Name it `pages.yml` and paste the contents of
   `deploy/pages-workflow.yml` (everything from `name:` down). Commit to `main`.
2. Repo → **Settings** → **Pages** → **Build and deployment** → **Source**:
   **GitHub Actions**.

The workflow builds `site/` with the pinned Jekyll (`~> 4.4`) from the Gemfile,
applies the project-page base path, and deploys with `actions/deploy-pages`. It
needs no API key or secret (it uses the built-in `GITHUB_TOKEN`), and runs on
every push to `main` that touches `site/**`. The site publishes at
`https://epiphenomena.github.io/cuento/`.
