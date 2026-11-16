package agent

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestExampleManifests keeps the manifests in examples/ and the labs
// valid against the current runtime, so the docs can't drift from it.
func TestExampleManifests(t *testing.T) {
	for _, dir := range []string{"../examples/llm-agent", "../labs/weather-agent"} {
		names, _ := os.ReadDir(dir)
		for _, n := range names {
			if !strings.HasSuffix(n.Name(), ".yaml") {
				continue
			}
			m, err := LoadManifest(os.DirFS(dir), n.Name())
			if err != nil {
				t.Errorf("%s/%s: %v", dir, n.Name(), err)
				continue
			}
			for _, p := range Check(m, os.DirFS(dir), false) {
				if !strings.Contains(p, "isn't in this binary") { // the lab's Go handler
					t.Errorf("%s/%s: %s", dir, n.Name(), p)
				}
			}
		}
	}
}

// TestLabSixSnippets appends lab 6's manifest snippets to the lab solution,
// as the lab does, and checks the result parses.
func TestLabSixSnippets(t *testing.T) {
	lab, err := os.ReadFile("../labs/06-extend.md")
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.ReadFile("../labs/weather-agent/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The solution already has the current handler; keep the rest.
	doc := string(base)
	if i := strings.Index(doc, "  # Lab 6:"); i >= 0 {
		doc = doc[:i]
	}
	re := regexp.MustCompile(`(?s)cat >> \$LAB/weather/agent\.yaml <<'EOF'\n(.*?)\nEOF\n`)
	snippets := re.FindAllStringSubmatch(string(lab), -1)
	if len(snippets) < 2 {
		t.Fatalf("found %d manifest snippets in lab 6, want 2", len(snippets))
	}
	for _, s := range snippets {
		doc += s[1] + "\n"
	}
	m, err := ParseManifest([]byte(doc))
	if err != nil {
		t.Fatalf("lab 6 manifest:\n%s\n%v", doc, err)
	}
	var llmHandlers int
	for _, h := range m.Handlers {
		if h.LLM != nil {
			llmHandlers++
		}
	}
	if llmHandlers != 1 {
		t.Errorf("lab 6 manifest has %d llm handlers, want 1", llmHandlers)
	}
}
