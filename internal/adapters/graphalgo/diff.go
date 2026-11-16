package graphalgo

import (
	"errors"
	"io"
)

// KeyReader yields one ordered key at a time. Next returns io.EOF when the
// stream is exhausted. Both streams of a diff must be in the same ascending
// order, which is what lets the merge hold one key per side instead of loading
// two id sets into memory.
type KeyReader interface {
	Next() (string, error)
}

// KeyDiff is the set difference of two ordered key streams.
type KeyDiff struct {
	// Added counts keys present in the head stream and absent from the base.
	Added int
	// Removed counts keys present in the base stream and absent from the head.
	Removed int
	// AddedKeys and RemovedKeys are the first SampleLimit keys of each side,
	// in stream order.
	AddedKeys   []string
	RemovedKeys []string
	// Truncated is true when at least one key was omitted from the samples.
	Truncated bool
}

// DiffKeys merges two ascending key streams and reports the set difference.
// Counts are exact even when the samples are capped, so a clipped sample never
// understates the delta. A key present on both sides is unchanged.
func DiffKeys(head, base KeyReader, sampleLimit int) (KeyDiff, error) {
	if sampleLimit < 0 {
		sampleLimit = 0
	}
	diff := KeyDiff{AddedKeys: make([]string, 0, sampleLimit), RemovedKeys: make([]string, 0, sampleLimit)}
	headKey, headErr := nextKey(head)
	baseKey, baseErr := nextKey(base)
	for {
		if headErr != nil && !errors.Is(headErr, io.EOF) {
			return KeyDiff{}, headErr
		}
		if baseErr != nil && !errors.Is(baseErr, io.EOF) {
			return KeyDiff{}, baseErr
		}
		headDone := errors.Is(headErr, io.EOF)
		baseDone := errors.Is(baseErr, io.EOF)
		switch {
		case headDone && baseDone:
			return diff, nil
		case baseDone || (!headDone && headKey < baseKey):
			diff.Added++
			if len(diff.AddedKeys) < sampleLimit {
				diff.AddedKeys = append(diff.AddedKeys, headKey)
			} else {
				diff.Truncated = true
			}
			headKey, headErr = nextKey(head)
		case headDone || baseKey < headKey:
			diff.Removed++
			if len(diff.RemovedKeys) < sampleLimit {
				diff.RemovedKeys = append(diff.RemovedKeys, baseKey)
			} else {
				diff.Truncated = true
			}
			baseKey, baseErr = nextKey(base)
		default:
			headKey, headErr = nextKey(head)
			baseKey, baseErr = nextKey(base)
		}
	}
}

// nextKey reads one key and normalizes a nil error with an empty key, which a
// reader must not produce but a broken one might.
func nextKey(reader KeyReader) (string, error) {
	if reader == nil {
		return "", io.EOF
	}
	return reader.Next()
}
