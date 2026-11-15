// Package agents holds two small demo agents for the a2a/v1 bus: a worker
// that serves a "textstats" capability and answers direct requests, and a
// coordinator that drives a scripted conversation with it and checks every
// reply. Both are deterministic (no LLM) so they double as a functional test.
package agents

import (
	"sort"
	"strings"
	"unicode"
)

// StatsRequest is the body of a textstats task.
type StatsRequest struct {
	Text string `json:"text"`
}

// StatsResult is the body of a textstats response.
type StatsResult struct {
	Words    int         `json:"words"`
	Chars    int         `json:"chars"`
	Lines    int         `json:"lines"`
	TopWords []WordCount `json:"top_words"`
	Worker   string      `json:"worker"`
}

// WordCount is one entry in StatsResult.TopWords.
type WordCount struct {
	Word  string `json:"word"`
	Count int    `json:"count"`
}

// Stats computes text statistics. It is what the worker runs per task and
// what the coordinator uses to check the worker's answer.
func Stats(text string) StatsResult {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '\''
	})
	counts := map[string]int{}
	for _, w := range words {
		counts[w]++
	}
	top := make([]WordCount, 0, len(counts))
	for w, n := range counts {
		top = append(top, WordCount{w, n})
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].Count != top[j].Count {
			return top[i].Count > top[j].Count
		}
		return top[i].Word < top[j].Word
	})
	if len(top) > 3 {
		top = top[:3]
	}
	lines := 0
	if text != "" {
		lines = strings.Count(text, "\n") + 1
	}
	return StatsResult{Words: len(words), Chars: len([]rune(text)), Lines: lines, TopWords: top}
}
