package main

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestContentSHA256_emptyReturnsEmpty(t *testing.T) {
	if got := contentSHA256(""); got != "" {
		t.Errorf("contentSHA256(\"\") = %q, want \"\"", got)
	}
}

func TestContentSHA256_deterministicAndHex(t *testing.T) {
	a := contentSHA256("hello world")
	b := contentSHA256("hello world")
	if a != b {
		t.Errorf("contentSHA256 not deterministic: %q vs %q", a, b)
	}
	want := hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte("hello world")); return s[:] }())
	if a != want {
		t.Errorf("contentSHA256 = %q, want %q (sha256 hex of input)", a, want)
	}
	if len(a) != 64 {
		t.Errorf("contentSHA256 length = %d, want 64 (hex of 32-byte digest)", len(a))
	}
}

func TestContentSHA256_distinctInputsDiffer(t *testing.T) {
	a := contentSHA256("foo")
	b := contentSHA256("bar")
	if a == b {
		t.Errorf("contentSHA256(%q) == contentSHA256(%q) = %q — must differ", "foo", "bar", a)
	}
}
