package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// RFC #249 (opentalon/opentalon#249) structured candidate types. These
// mirror the Go structs the orchestrator unmarshals from the
// `knowledge_candidates` / `glossary_candidates` / `tool_candidates`
// JSON fields on a preparer response — emitting them alongside the
// legacy `message` envelope and `relevant_tools` slice lets the
// orchestrator's Phase 3 dedup and Phase 4 tier-decision logic apply
// per-article / per-tool identity rules. Without the structured slices
// the orchestrator atomically demotes the entire turn to
// `mode=legacy_fallback` and skips both pillars (intentional safety
// gate, see knowledge_dedup.go:responseUsesLegacyKnowledgeInjection in
// opentalon core).
//
// Field names + JSON tags must stay in lock-step with the orchestrator
// side; the canonical source of truth is
// opentalon/internal/orchestrator/orchestrator.go (the KnowledgeCandidate
// / GlossaryCandidate / ToolCandidate types). Drift between the two
// repos shows up as silent zero-value fields on the consumer end.

// KnowledgeCandidate is one knowledge-base article returned by the
// preparer's RAG search.
type KnowledgeCandidate struct {
	ArticleID         string  `json:"article_id"`
	Title             string  `json:"title,omitempty"`
	Content           string  `json:"content"`
	ContentSHA256     string  `json:"content_sha256,omitempty"`
	Score             float64 `json:"score"`
	Source            string  `json:"source,omitempty"`
	PositionInResults int     `json:"position_in_results,omitempty"`
}

// GlossaryCandidate mirrors KnowledgeCandidate; the only difference is
// the per-entry "term" instead of "title".
type GlossaryCandidate struct {
	Term              string  `json:"term"`
	Content           string  `json:"content"`
	ContentSHA256     string  `json:"content_sha256,omitempty"`
	Score             float64 `json:"score"`
	Source            string  `json:"source,omitempty"`
	PositionInResults int     `json:"position_in_results,omitempty"`
}

// ToolCandidate is the RAG-retrieved tool entry with score so the
// orchestrator's Phase 4 tier-decision can apply LRU + tier-promotion
// logic and emit a ranked tool_retrieval event. ToolName uses the
// "plugin.action" form the orchestrator's relevant_tools filter expects.
type ToolCandidate struct {
	ToolName          string  `json:"tool_name"`
	Score             float64 `json:"score"`
	PositionInResults int     `json:"position_in_results,omitempty"`
}

// extractKnowledgeCandidates converts the GraphQL response items
// (post-filterOutMCPItems) into KnowledgeCandidate structs. Items below
// minScore are dropped — same filter the legacy [knowledge_context]
// block applies — so the orchestrator never sees a candidate it would
// have ignored on the legacy path. PositionInResults is 1-indexed and
// reflects the post-filter rank that the LLM actually saw.
//
// ArticleID falls back to the Weaviate-internal `_additional.id` when
// the article has no plugin-supplied identifier; ContentSHA256 hashes
// the same title+content body that the renderer would emit so a
// re-injection of the same article across turns produces an identical
// dedup key.
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

// extractGlossaryCandidates mirrors extractKnowledgeCandidates for the
// Glossary collection. ContentSHA256 hashes the same term+definition
// body that the legacy formatGlossaryItems renderer would emit, so
// future glossary dedup (if added) sees a stable identity.
func extractGlossaryCandidates(items []map[string]interface{}, minScore float64) []GlossaryCandidate {
	if len(items) == 0 {
		return nil
	}
	out := make([]GlossaryCandidate, 0, len(items))
	for _, item := range items {
		if !aboveScore(item, minScore) {
			continue
		}
		term, _ := item["term"].(string)
		definition, _ := item["definition"].(string)
		if term == "" || definition == "" {
			continue
		}
		source, _ := item["source"].(string)
		body := definition
		out = append(out, GlossaryCandidate{
			Term:              term,
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

// actionFilter is the single chokepoint for MCPActions retrieval filtering.
// Every filter axis the prepare pipeline applies to a GraphQL result item
// lives here:
//
//   - minScore         per-collection cutoff (configurable via
//     min_prepare_score_tools, defaulting to legacy)
//   - availableTools   per-session tool palette (the host injects an
//     `allowed_tools` JSON array via the ContextArgProvider
//     registry; nil here means "no per-session filter")
//
// New axes (e.g. recency, tier-aware boosts) get a field on this struct,
// not a second parallel filter loop in each extractor. The zero value
// {minScore: 0, availableTools: nil} accepts every well-formed item — the
// right default for callers that don't care about either axis.
type actionFilter struct {
	minScore       float64
	availableTools map[string]struct{}
}

func (f actionFilter) accept(pluginName, actionName string, item map[string]interface{}) bool {
	if !aboveScore(item, f.minScore) {
		return false
	}
	if f.availableTools != nil {
		fqn := pluginName + "." + actionName
		if _, ok := f.availableTools[fqn]; !ok {
			return false
		}
	}
	return true
}

// walkRetrievedActions iterates the MCPActions GraphQL response and calls fn
// for each item whose (plugin, action, score, palette) tuple passes the
// filter. Single iteration point used by both name- and candidate-extractors
// so a new filter axis lives in actionFilter, not in two parallel loops.
func walkRetrievedActions(result interface{}, className string, filter actionFilter, fn func(pluginName, actionName string, item map[string]interface{})) {
	for _, item := range extractItems(result, className) {
		pluginName, _ := item["pluginName"].(string)
		actionName, _ := item["actionName"].(string)
		if pluginName == "" || actionName == "" {
			continue
		}
		if !filter.accept(pluginName, actionName, item) {
			continue
		}
		fn(pluginName, actionName, item)
	}
}

// Both extractors below funnel through walkRetrievedActions and return
// non-nil empty slices on "filter ran, found nothing". Symmetric helper
// surface — nil-vs-empty drift between the two would let downstream code
// nil-check one and len-check the other, exactly the inconsistency the
// chokepoint refactor exists to prevent. Callers that need a nil signal
// (e.g. prepare's "no relevant tools active") normalize at the call site,
// not in these helpers.

// extractToolNames returns the "<plugin>.<action>" FQNs from an MCPActions
// GraphQL result that pass the filter. Returns a non-nil empty slice when
// no item matches.
func extractToolNames(result interface{}, className string, filter actionFilter) []string {
	tools := []string{}
	walkRetrievedActions(result, className, filter, func(p, a string, _ map[string]interface{}) {
		tools = append(tools, p+"."+a)
	})
	return tools
}

// extractToolCandidatesFromResult walks the GraphQL response for the
// MCPActions collection and produces ranked ToolCandidate entries through
// the shared actionFilter chokepoint. PositionInResults is 1-indexed and
// reflects the post-filter rank. Returns a non-nil empty slice when no
// item matches (see the symmetry note above).
func extractToolCandidatesFromResult(result interface{}, className string, filter actionFilter) []ToolCandidate {
	out := []ToolCandidate{}
	walkRetrievedActions(result, className, filter, func(p, a string, item map[string]interface{}) {
		out = append(out, ToolCandidate{
			ToolName:          p + "." + a,
			Score:             scoreOf(item),
			PositionInResults: len(out) + 1,
		})
	})
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
