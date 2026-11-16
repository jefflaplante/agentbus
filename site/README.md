# The documentation site

A single-page site for GitHub Pages, built from the repository's own
markdown. Nothing on it is written twice: the docs pages are README.md,
CONTRACT.md, docs/, examples/ and labs/ as they are in the repository.

```text
site/
├── build/main.go     renders the markdown to docs.json and copies the images
└── static/           the page itself, copied as is
    ├── index.html    the home page and the docs reader's frame
    └── assets/
        ├── hero.js       the broker animation and the live wire
        ├── app.js        hash routing, the docs reader, search, tabs
        ├── highlight.js  syntax colors for go, python, json, yaml, bash
        └── site.css
```

## Build and preview

```bash
cd site
go run ./build -repo .. -out ../_site
python3 -m http.server 8765 --directory ../_site
```

Then open http://localhost:8765. `-strict` fails the build on a link to a
missing file or a missing image; CI runs it on every push and pull
request.

`site/` is its own Go module, so goldmark, the markdown renderer, is not
a dependency of agentbus.

## How the build works

- **Pages** are listed in `pages` in `build/main.go`, in sidebar order,
  with the group and title each gets. A new markdown file needs a line
  there to appear on the site.
- **Links** between listed pages become routes such as
  `#/docs/cli/onboarding-an-agent`. Links to any other file in the
  repository go to it on GitHub.
- **Heading IDs** follow GitHub's rules, so an anchor written for
  github.com works here too.
- **Images** a page uses are copied to `r/<path in the repository>`.
- **The lab video**, a bare GitHub attachment URL in the markdown, becomes
  a video player.

## Deploying

`.github/workflows/pages.yml` builds and publishes the site on every push
to main that touches markdown, `docs/images/` or `site/`. The repository's
Pages source has to be set to "GitHub Actions" once, under
Settings → Pages.
