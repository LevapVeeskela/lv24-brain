package domain

import (
	"testing"
)

func TestNormalizeDedupTextFoldsCasePunctuationAndYo(t *testing.T) {
	raw := "## Ещё **раз**: `brain`  review —\n\t[ссылка](x) ЁЛКА!"
	if got, want := NormalizeDedupText(raw), "еще раз brain review ссылка x елка"; got != want {
		t.Fatalf("NormalizeDedupText = %q, want %q", got, want)
	}
	if got := NormalizeDedupText("  \n\t "); got != "" {
		t.Fatalf("NormalizeDedupText(blank) = %q", got)
	}
}

func TestBodyFingerprintIsDeterministicAndNoiseInsensitive(t *testing.T) {
	base := BodyFingerprint("Releases go through OIDC; no tokens.")
	again := BodyFingerprint("releases   GO through oidc no tokens")
	if base == "" || base != again {
		t.Fatalf("fingerprints differ: %q vs %q", base, again)
	}
	if len(base) != 64 {
		t.Fatalf("fingerprint length = %d, want 64 hex chars", len(base))
	}
	if other := BodyFingerprint("releases go through a stored token"); other == base {
		t.Fatal("different texts share a fingerprint")
	}
}

func TestSimhash64IsDeterministicAndOrderIndependent(t *testing.T) {
	text := "index search rebuilt sync old removed new written disk worker rebuild schema"
	if first, second := Simhash64(text), Simhash64(text); first != second {
		t.Fatalf("simhash is not deterministic: %d vs %d", first, second)
	}
	reordered := "schema rebuild worker disk written new removed old sync rebuilt search index"
	if a, b := Simhash64(text), Simhash64(reordered); a != b {
		t.Fatalf("same words reordered sketched differently: %d vs %d", a, b)
	}
	if got := Simhash64(""); got != 0 {
		t.Fatalf("empty text simhash = %d, want 0", got)
	}
}

func TestHammingDistanceSeparatesNearFromUnrelated(t *testing.T) {
	nearA := Simhash64("index search rebuilt sync old removed new written disk worker rebuild schema")
	nearB := Simhash64("schema rebuild worker disk written new removed old sync rebuilt search index")
	if distance := HammingDistance(nearA, nearB); distance > NearDuplicateHammingDistance {
		t.Fatalf("reordered words distance = %d, want <= %d", distance, NearDuplicateHammingDistance)
	}
	unrelatedA := Simhash64("publish the npm package through trusted publishing with oidc tokens and provenance")
	unrelatedB := Simhash64("rebuild the sqlite search index when the schema version changes on disk again")
	if distance := HammingDistance(unrelatedA, unrelatedB); distance <= NearDuplicateHammingDistance {
		t.Fatalf("unrelated texts distance = %d, want > %d", distance, NearDuplicateHammingDistance)
	}
	if HammingDistance(unrelatedA, unrelatedB) != HammingDistance(unrelatedB, unrelatedA) {
		t.Fatal("HammingDistance is not symmetric")
	}
	if HammingDistance(unrelatedA, unrelatedA) != 0 {
		t.Fatal("HammingDistance(a, a) != 0")
	}
}

func TestWordCountUsesNormalizedWords(t *testing.T) {
	if got := WordCount("**One**, two! three?"); got != 3 {
		t.Fatalf("WordCount = %d, want 3", got)
	}
	if got := WordCount(" , "); got != 0 {
		t.Fatalf("WordCount(blank) = %d, want 0", got)
	}
}
