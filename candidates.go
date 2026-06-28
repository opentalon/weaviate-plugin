package main

import (
	"crypto/sha256"
	"encoding/hex"
)

// additionalID pulls _additional.id off a Weaviate GraphQL result. The
// graphql client returns _additional as a map[string]interface{}; a
// missing or non-string id yields the empty string.
func additionalID(item map[string]interface{}) string {
	add, ok := item["_additional"].(map[string]interface{})
	if !ok {
		return ""
	}
	id, _ := add["id"].(string)
	return id
}

// contentSHA256 returns the lowercase hex sha256 of s. Empty input
// produces the empty string so a doc with no body doesn't get a sentinel
// hash as its change-detection key.
func contentSHA256(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
