package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// Structured candidate types. KnowledgeCandidate mirrors the Go struct the
// orchestrator unmarshals from the `knowledge_candidates` JSON field on an
// ask_knowledge response — per-article identity (ArticleID + ContentSHA256)
// lets the orchestrator's dedup logic apply per-article rules. Field names +
// JSON tags must stay in lock-step with the orchestrator side.

// KnowledgeCandidate is one knowledge-base article returned by a semantic
// knowledge search.
type KnowledgeCandidate struct {
	ArticleID         string  `json:"article_id"`
	Title             string  `json:"title,omitempty"`
	Content           string  `json:"content"`
	ContentSHA256     string  `json:"content_sha256,omitempty"`
	Score             float64 `json:"score"`
	Source            string  `json:"source,omitempty"`
	PositionInResults int     `json:"position_in_results,omitempty"`
}

// extractKnowledgeCandidates converts the GraphQL response items (already
// scoped to the knowledge collection) into KnowledgeCandidate structs. Items
// below minScore are dropped. PositionInResults is 1-indexed and reflects the
// post-filter rank.
//
// ArticleID falls back to the Weaviate-internal `_additional.id` when the
// article has no plugin-supplied identifier; ContentSHA256 hashes the same
// title+content body so a re-injection of the same article across turns
// produces an identical dedup key.
func extractKnowledgeCandidates(items []map[string]interface{}, minScore float64) []KnowledgeCandidate {
	if len(items) == 0 {
		return nil
	}
	out := make([]KnowledgeCandidate, 0, len(items))
	for _, item := range items {
		if !aboveScore(item, minScore) {
			continue
		}
		title, _ := item["title"].(string)
		content, _ := item["content"].(string)
		if title == "" && content == "" {
			continue
		}
		source, _ := item["source"].(string)
		body := joinTitleAndContent(title, content)
		out = append(out, KnowledgeCandidate{
			ArticleID:         additionalID(item),
			Title:             title,
			Content:           body,
			ContentSHA256:     contentSHA256(body),
			Score:             scoreOf(item),
			Source:            source,
			PositionInResults: len(out) + 1,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// additionalID pulls _additional.id off a Weaviate GraphQL result. The
// graphql client returns _additional as a map[string]interface{}; a
// missing or non-string id yields the empty string, which the
// orchestrator treats as "use ContentSHA256 as the only identity".
func additionalID(item map[string]interface{}) string {
	add, ok := item["_additional"].(map[string]interface{})
	if !ok {
		return ""
	}
	id, _ := add["id"].(string)
	return id
}

// scoreOf reads _additional.score. The graphql client deserializes
// numeric scores as string (the only shape Weaviate returns); a missing
// or unparseable value yields 0. This deliberately differs from
// aboveScore's "missing → include by default" semantics: aboveScore
// gates a candidate's visibility, while scoreOf reports the actual
// score we ship downstream — and "we don't know" is more honestly
// represented as 0.0 than as a fabricated threshold-passing value.
func scoreOf(item map[string]interface{}) float64 {
	add, ok := item["_additional"].(map[string]interface{})
	if !ok {
		return 0
	}
	scoreStr, _ := add["score"].(string)
	if scoreStr == "" {
		return 0
	}
	score, err := strconv.ParseFloat(scoreStr, 64)
	if err != nil {
		return 0
	}
	return score
}

// contentSHA256 returns the lowercase hex sha256 of s. Empty input
// produces the empty string so a candidate with no body (rare —
// extract* helpers already drop those) doesn't leak a sentinel hash
// downstream as the dedup key for "nothing".
func contentSHA256(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// joinTitleAndContent produces the renderable body for a knowledge
// article: title and content separated by a blank line, with each
// half trimmed of trailing whitespace so the dedup key is stable
// across minor formatting jitter. Either half may be empty (the
// caller already enforced "at least one non-empty"), in which case
// only the non-empty half is returned.
func joinTitleAndContent(title, content string) string {
	t := strings.TrimRight(title, " \t\r\n")
	c := strings.TrimRight(content, " \t\r\n")
	switch {
	case t == "" && c == "":
		return ""
	case t == "":
		return c
	case c == "":
		return t
	default:
		return t + "\n\n" + c
	}
}
