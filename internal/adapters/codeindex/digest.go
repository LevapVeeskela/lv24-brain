package codeindex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

// graphDigestLength is the hex length of the digest stored in meta. A full
// sha256 is not needed: the value only has to distinguish graph snapshots of
// one store, and 16 bytes is far beyond collision risk at this scale.
const graphDigestLength = 32

// graphDigest hashes the symbol id set and the distinct dependency pairs of
// the current transaction. It is the change detector of the community
// partition: a sync that leaves the graph unchanged (a comment edit, a body
// edit without a new edge) reuses the stored partition instead of running
// Louvain again.
//
// The digest deliberately ignores line numbers, edge kinds and repeated call
// sites: none of them changes the undirected graph the partition is computed
// from. Only directed pair identity and the symbol set matter.
func graphDigest(ctx context.Context, db queryer) (string, error) {
	hash := sha256.New()
	write := func(value string) {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	// The algorithm name covers the hub percentile and every tuning constant;
	// bump CommunityAlgorithm when one of them changes so a stored partition
	// is not reused by a different algorithm.
	write("algorithm=" + CommunityAlgorithm)

	symbolRows, err := db.QueryContext(ctx, `SELECT id FROM symbols ORDER BY id`)
	if err != nil {
		return "", fmt.Errorf("codeindex: digest symbols: %w", err)
	}
	symbols := 0
	for symbolRows.Next() {
		var id string
		if err := symbolRows.Scan(&id); err != nil {
			_ = symbolRows.Close()
			return "", fmt.Errorf("codeindex: digest symbols: %w", err)
		}
		write(id)
		symbols++
	}
	rowErr := symbolRows.Err()
	_ = symbolRows.Close()
	if rowErr != nil {
		return "", fmt.Errorf("codeindex: digest symbols: %w", rowErr)
	}

	write("symbols=" + strconv.Itoa(symbols))
	arguments := stringsToAny(traversalEdgeKinds)
	edgeRows, err := db.QueryContext(ctx,
		`SELECT DISTINCT source, target FROM edges
		 WHERE kind IN (`+placeholders(len(traversalEdgeKinds))+`)
		 ORDER BY source, target`, arguments...)
	if err != nil {
		return "", fmt.Errorf("codeindex: digest edges: %w", err)
	}
	edges := 0
	for edgeRows.Next() {
		var source, target string
		if err := edgeRows.Scan(&source, &target); err != nil {
			_ = edgeRows.Close()
			return "", fmt.Errorf("codeindex: digest edges: %w", err)
		}
		if source == target {
			continue
		}
		write(source)
		write(target)
		edges++
	}
	rowErr = edgeRows.Err()
	_ = edgeRows.Close()
	if rowErr != nil {
		return "", fmt.Errorf("codeindex: digest edges: %w", rowErr)
	}
	write("edges=" + strconv.Itoa(edges))
	return hex.EncodeToString(hash.Sum(nil))[:graphDigestLength], nil
}

// readGraphDigest returns the digest and community count stored by the last
// partition rebuild. Missing keys yield an empty digest and a zero count.
func readGraphDigest(ctx context.Context, tx *sql.Tx) (string, int, error) {
	var digest string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaGraphDigest).Scan(&digest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", 0, fmt.Errorf("codeindex: read graph digest: %w", err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM communities`).Scan(&count); err != nil {
		return "", 0, fmt.Errorf("codeindex: count communities: %w", err)
	}
	return digest, count, nil
}
