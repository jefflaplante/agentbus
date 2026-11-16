// Command build turns the repository's markdown into the documentation
// site: static/ copied as is, every page in pages rendered to HTML into
// docs.json, and the images those pages use copied beside it.
//
//	go run ./build -repo .. -out ../_site
//	go run ./build -strict ...   # fail on a broken link or missing image
//
// Links between pages become routes (#/docs/cli/onboarding-an-agent),
// links to other repository files go to GitHub, and heading IDs follow
// GitHub's rules, so anchors written for github.com work here too.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
)

const repoURL = "https://github.com/jefflaplante/agentbus"

// videos maps a GitHub video attachment, as the markdown links it, to the
// copy the site serves. An attachment URL only plays on github.com, which
// swaps in a signed URL while rendering; anywhere else it is a 404. The
// copies aren't in git: they're on the lab-video release, and the Pages
// build downloads them into static/media before building.
var videos = map[string]struct{ src, poster string }{
	"https://github.com/user-attachments/assets/b4ccd343-cc8e-4803-a1c3-5d4623122314": {
		"media/agentbus-lab.mp4", "media/agentbus-lab-poster.png",
	},
}

// page is one markdown file on the site, in sidebar order.
type page struct {
	Path, Slug, Group, Title string
}

var pages = []page{
	{"README.md", "overview", "Start", "Overview"},
	{"demo/README.md", "demo", "Start", "The demo"},
	{"docs/HOW-IT-WORKS.md", "how-it-works", "Start", "How it works"},
	{"docs/CLI.md", "cli", "Guides", "The CLI and console"},
	{"docs/SERVICES.md", "services", "Guides", "Writing a service agent"},
	{"docs/AGENTS.md", "agents", "Guides", "Standalone agents"},
	{"docs/INTEGRATIONS.md", "integrations", "Guides", "Coding agents and MCP"},
	{"docs/DESKTOP-CLIENTS.md", "desktop-clients", "Guides", "Desktop MQTT clients"},
	{"CONTRACT.md", "contract", "Reference", "The contract"},
	{"docs/SECURITY.md", "security", "Reference", "Security"},
	{"docs/RETENTION.md", "retention", "Reference", "Retention"},
	{"examples/python/README.md", "python", "Examples", "Python"},
	{"examples/systemd/README.md", "systemd", "Examples", "systemd units"},
	{"examples/integrations/skills/agentbus/SKILL.md", "skill", "Examples", "Claude Code skill"},
	{"labs/README.md", "labs", "Labs", "Workshop"},
	{"labs/00-setup.md", "lab-0", "Labs", "0 · Setup"},
	{"labs/01-broker.md", "lab-1", "Labs", "1 · Start a broker"},
	{"labs/02-first-agent.md", "lab-2", "Labs", "2 · A first agent"},
	{"labs/03-weather-agent.md", "lab-3", "Labs", "3 · The weather agent"},
	{"labs/04-build-deploy.md", "lab-4", "Labs", "4 · Build and deploy"},
	{"labs/05-claude-code.md", "lab-5", "Labs", "5 · Claude Code"},
	{"labs/06-extend.md", "lab-6", "Labs", "6 · Extend it"},
	{"labs/TEACHING.md", "teaching", "Labs", "Teaching script"},
	{"labs/weather-agent/README.md", "weather-agent", "Labs", "Weather agent source"},
	{"labs/video/README.md", "video", "Labs", "The lab video"},
}

// Heading is one entry of a page's table of contents.
type Heading struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Level int    `json:"level"`
}

// Doc is one rendered page in docs.json.
type Doc struct {
	Slug     string    `json:"slug"`
	Title    string    `json:"title"`
	Group    string    `json:"group"`
	Source   string    `json:"source"`
	HTML     string    `json:"html"`
	Text     string    `json:"text"` // plain text, for search
	Headings []Heading `json:"headings"`
}

func main() {
	repo := flag.String("repo", "..", "repository root")
	out := flag.String("out", "../_site", "output directory")
	strict := flag.Bool("strict", false, "fail on a broken link or a missing image")
	flag.Parse()
	warnings, err := build(*repo, *out)
	if err != nil {
		log.Fatal(err)
	}
	if *strict && warnings > 0 {
		log.Fatalf("%d warnings with -strict", warnings)
	}
}

func build(repo, out string) (int, error) {
	if err := os.RemoveAll(out); err != nil {
		return 0, err
	}
	if err := copyTree(filepath.Join(repo, "site", "static"), out); err != nil {
		return 0, err
	}
	bySource := map[string]string{}
	for _, p := range pages {
		bySource[p.Path] = p.Slug
	}
	b := &builder{repo: repo, out: out, bySource: bySource, copied: map[string]bool{}}
	var docs []Doc
	for _, p := range pages {
		d, err := b.render(p)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", p.Path, err)
		}
		docs = append(docs, d)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{"repo": repoURL, "docs": docs}); err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(out, "docs.json"), buf.Bytes(), 0o644); err != nil {
		return 0, err
	}
	// GitHub Pages runs Jekyll on the artifact unless told not to.
	if err := os.WriteFile(filepath.Join(out, ".nojekyll"), nil, 0o644); err != nil {
		return 0, err
	}
	log.Printf("built %d pages, %d images, %d KB of docs.json into %s", len(docs), len(b.copied), buf.Len()/1024, out)
	for _, w := range b.warnings {
		log.Print("warning: ", w)
	}
	return len(b.warnings), nil
}

type builder struct {
	repo, out string
	bySource  map[string]string
	copied    map[string]bool
	warnings  []string
}

func (b *builder) render(p page) (Doc, error) {
	src, err := os.ReadFile(filepath.Join(b.repo, p.Path))
	if err != nil {
		return Doc{}, err
	}
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(html.WithUnsafe()),
	)
	ctx := parser.NewContext(parser.WithIDs(&githubIDs{seen: map[string]int{}}))
	doc := md.Parser().Parse(text.NewReader(src), parser.WithContext(ctx))

	d := Doc{Slug: p.Slug, Title: p.Title, Group: p.Group, Source: p.Path, Headings: []Heading{}}
	var plain strings.Builder
	var firstH1 ast.Node
	err = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Heading:
			if n.Level == 1 && firstH1 == nil {
				firstH1 = n // the page title is the sidebar's; drop the H1
			}
			if id, ok := n.AttributeString("id"); ok && n.Level >= 2 && n.Level <= 3 {
				d.Headings = append(d.Headings, Heading{ID: string(id.([]byte)), Text: nodeText(n, src), Level: n.Level})
			}
		case *ast.Link:
			n.Destination = []byte(b.link(p.Path, string(n.Destination)))
		case *ast.Image:
			n.Destination = []byte(b.image(p.Path, string(n.Destination)))
		case *ast.AutoLink:
			// A bare GitHub video attachment is a player on github.com. Here
			// it plays the site's own copy, or links to GitHub without one.
			u := string(n.URL(src))
			if strings.HasPrefix(u, "https://github.com/user-attachments/assets/") {
				player := fmt.Sprintf(`<a class="video-link" href="%s">Watch the video on GitHub</a>`, u)
				v, ok := videos[u]
				if ok && !exists(filepath.Join(b.repo, "site", "static", filepath.FromSlash(v.src))) {
					b.warnings = append(b.warnings, fmt.Sprintf("%s: %s is missing; it links to GitHub instead "+
						"(gh release download lab-video --dir site/static/media)", p.Path, v.src))
					ok = false
				}
				if ok {
					player = fmt.Sprintf(`<video class="doc-video" controls playsinline preload="metadata" poster="%s" src="%s"></video>`, v.poster, v.src)
				} else if _, known := videos[u]; !known {
					b.warnings = append(b.warnings, fmt.Sprintf("%s: video %s has no copy in site/static/media; it links to GitHub", p.Path, u))
				}
				video := ast.NewString([]byte(player))
				video.SetCode(true) // written as is, not escaped
				n.Parent().ReplaceChild(n.Parent(), n, video)
				return ast.WalkSkipChildren, nil
			}
		case *ast.Text:
			plain.Write(n.Value(src))
			plain.WriteByte(' ')
		case *ast.String:
			if !n.IsCode() {
				plain.Write(n.Value)
				plain.WriteByte(' ')
			}
		case *ast.CodeSpan:
			plain.WriteString(nodeText(n, src) + " ")
			return ast.WalkSkipChildren, nil
		case *ast.FencedCodeBlock:
			lines := n.Lines()
			for i := 0; i < lines.Len(); i++ {
				seg := lines.At(i)
				plain.Write(seg.Value(src))
			}
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return Doc{}, err
	}
	if firstH1 != nil {
		firstH1.Parent().RemoveChild(firstH1.Parent(), firstH1)
	}
	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, src, doc); err != nil {
		return Doc{}, err
	}
	d.HTML = buf.String()
	d.Text = strings.Join(strings.Fields(plain.String()), " ")
	return d, nil
}

// link rewrites a link found in the page at from.
func (b *builder) link(from, dest string) string {
	u, err := url.Parse(dest)
	if err != nil || u.Scheme != "" || dest == "" {
		return dest
	}
	if u.Path == "" { // #fragment on this page
		return "#/docs/" + b.bySource[from] + "/" + u.Fragment
	}
	target := path.Clean(path.Join(path.Dir(from), u.Path))
	if slug, ok := b.bySource[target]; ok {
		return docRoute(slug, u.Fragment)
	}
	if slug, ok := b.bySource[path.Join(target, "README.md")]; ok {
		return docRoute(slug, u.Fragment)
	}
	info, err := os.Stat(filepath.Join(b.repo, target))
	if err != nil {
		b.warnings = append(b.warnings, fmt.Sprintf("%s links to missing %s", from, target))
		return repoURL + "/tree/main/" + target
	}
	kind := "blob"
	if info.IsDir() {
		kind = "tree"
	}
	s := repoURL + "/" + kind + "/main/" + target
	if u.Fragment != "" {
		s += "#" + u.Fragment
	}
	return s
}

func docRoute(slug, frag string) string {
	if frag == "" {
		return "#/docs/" + slug
	}
	return "#/docs/" + slug + "/" + frag
}

// image copies a repository image into the site and returns its new path.
func (b *builder) image(from, dest string) string {
	u, err := url.Parse(dest)
	if err != nil || u.Scheme != "" {
		return dest
	}
	target := path.Clean(path.Join(path.Dir(from), u.Path))
	if !b.copied[target] {
		data, err := os.ReadFile(filepath.Join(b.repo, target))
		if err != nil {
			b.warnings = append(b.warnings, fmt.Sprintf("%s uses missing image %s", from, target))
			return dest
		}
		dst := filepath.Join(b.out, "r", filepath.FromSlash(target))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			b.warnings = append(b.warnings, err.Error())
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			b.warnings = append(b.warnings, err.Error())
		}
		b.copied[target] = true
	}
	return "r/" + target
}

func nodeText(n ast.Node, src []byte) string {
	var s strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if t, ok := c.(*ast.Text); ok {
				s.Write(t.Value(src))
			}
			if t, ok := c.(*ast.String); ok {
				s.Write(t.Value)
			}
		}
		return ast.WalkContinue, nil
	})
	return s.String()
}

// githubIDs makes heading IDs the way github.com does: lowercase, spaces
// to hyphens, punctuation other than - and _ dropped, -1, -2 for repeats.
type githubIDs struct{ seen map[string]int }

var notID = regexp.MustCompile(`[^\p{L}\p{N}\- _]`)

func (g *githubIDs) Generate(value []byte, kind ast.NodeKind) []byte {
	s := strings.ToLower(notID.ReplaceAllString(string(value), ""))
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return '-'
		}
		return r
	}, s)
	id := s
	if n, ok := g.seen[s]; ok {
		id = fmt.Sprintf("%s-%d", s, n)
	}
	g.seen[s]++
	return []byte(id)
}

func (g *githubIDs) Put(value []byte) { g.seen[string(value)]++ }

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
