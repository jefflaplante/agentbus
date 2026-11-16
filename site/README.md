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
- **The lab video** is a bare GitHub attachment URL in the markdown. That
  URL only plays on github.com, which swaps in a signed URL while
  rendering; from any other site it is a 404. So the site serves its own
  copy, and `videos` in `build/main.go` maps the attachment URL to it.

## Media

Videos and recordings are not in git. They are assets on two GitHub
releases, and the Pages build downloads them into `static/media/`
(gitignored) before building:

| Release | Files | Used by |
|---|---|---|
| [`lab-video`](https://github.com/jefflaplante/agentbus/releases/tag/lab-video) | `agentbus-lab.mp4`, `agentbus-lab-poster.png` | the Labs section and the labs pages |
| [`console-map`](https://github.com/jefflaplante/agentbus/releases/tag/console-map) | `agentbus-console-map.gif`, `.mp4`, `.png` | the README (the GIF) and the Console section (the MP4) |

The README loads its GIF from the site
(`https://code.jefflaplante.com/agentbus/media/agentbus-console-map.gif`),
not from the release: a release download is served as
`application/octet-stream`, which GitHub's image proxy refuses to show.

To build locally with them:

```bash
gh release download lab-video --dir site/static/media
gh release download console-map --dir site/static/media
```

Without them, the players fall back to a link to GitHub and to the Map
screenshot.

**Replacing the lab video** after rebuilding it (labs/video):

```bash
cp labs/video/out/agentbus-lab-web.mp4 agentbus-lab.mp4
cp labs/video/out/poster.png agentbus-lab-poster.png
gh release upload lab-video agentbus-lab.mp4 agentbus-lab-poster.png --clobber
```

**Re-recording the console map.** `tools/trafficgen` runs a broker with
agents that keep talking. Record the console against it with asciinema,
cut a 20-second window, and render it with agg and ffmpeg:

```bash
go run ./tools/trafficgen -listen 127.0.0.1:18960 -token /tmp/admin.token &
asciinema rec --headless --window-size 112x32 \
  -c 'agentbus --url tcp://127.0.0.1:18960 --agent admin --token-file /tmp/admin.token console --tab map' map.cast
# stop the console after half a minute or so: kill -TERM <its pid>
agg --font-family "JetBrains Mono,Menlo,Apple Braille" --font-size 16 --fps-cap 15 \
  --theme 0b0f18,e4e4e4,1b1f2a,ff6b6b,3dd68c,f5d547,5eb1ff,f07ac2,5ce1e6,e4e4e4 map.cast agentbus-console-map.gif
ffmpeg -i agentbus-console-map.gif -movflags +faststart -pix_fmt yuv420p \
  -vf "scale=trunc(iw/2)*2:trunc(ih/2)*2" -c:v libx264 -crf 22 agentbus-console-map.mp4
gh release upload console-map agentbus-console-map.gif agentbus-console-map.mp4 agentbus-console-map.png --clobber
```

The console redraws only what changes, so cutting a window out of the
cast has to keep the output before it; a cast trimmed naively starts on a
half-drawn screen. Then rerun the Pages workflow.

## Deploying

`.github/workflows/pages.yml` builds and publishes the site on every push
to main that touches markdown, `docs/images/` or `site/`. The repository's
Pages source has to be set to "GitHub Actions" once, under
Settings → Pages.
