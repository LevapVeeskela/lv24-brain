package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"math/bits"
	"strings"
	"unicode"
)

// NearDuplicateHammingDistance is the simhash distance at which two texts of at
// least SimhashMinWords words count as near-duplicates.
const (
	// NearDuplicateHammingDistance is the documented Hamming threshold.
	NearDuplicateHammingDistance = 3
	// SimhashMinWords keeps short texts out of the simhash comparison: with
	// only a few words the 64-bit sketch is too coarse to distinguish
	// "fix tests" from "fix tests now".
	SimhashMinWords = 8
)

// NormalizeDedupText casefolds a text, folds ё to е, drops punctuation and
// Markdown markup (every rune that is not a letter or digit) and collapses
// whitespace. Two texts that normalize to the same string are exact
// duplicates: punctuation, casing and layout carry no meaning.
func NormalizeDedupText(text string) string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return strings.ReplaceAll(strings.Join(words, " "), "ё", "е")
}

// BodyFingerprint is the SHA-256 of the normalized text, hex encoded. It is the
// exact-duplicate key: deterministic across machines, records and processes.
func BodyFingerprint(text string) string {
	sum := sha256.Sum256([]byte(NormalizeDedupText(text)))
	return hex.EncodeToString(sum[:])
}

// Simhash64 sketches a text into 64 bits, so two texts about the same thing
// land within a few bits of each other. The features are the normalized words
// with equal weight, which makes the sketch order-independent and stable
// across runs; an empty text sketches to zero.
func Simhash64(text string) uint64 {
	words := strings.Fields(NormalizeDedupText(text))
	if len(words) == 0 {
		return 0
	}
	weights := make([]int, 64)
	for _, word := range words {
		hasher := fnv.New64a()
		_, _ = hasher.Write([]byte(word))
		hash := hasher.Sum64()
		for bit := 0; bit < 64; bit++ {
			if hash&(uint64(1)<<uint(bit)) != 0 {
				weights[bit]++
			} else {
				weights[bit]--
			}
		}
	}
	var sketch uint64
	for bit := 0; bit < 64; bit++ {
		if weights[bit] > 0 {
			sketch |= uint64(1) << uint(bit)
		}
	}
	return sketch
}

// HammingDistance counts the bits two simhashes differ in.
func HammingDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// WordCount is the number of normalized words of a text; near-duplicate
// detection uses it to skip texts that are too short for a simhash.
func WordCount(text string) int {
	return len(strings.Fields(NormalizeDedupText(text)))
}
