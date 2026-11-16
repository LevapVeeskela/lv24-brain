package app

// RecordPath returns the logical data-directory path of a curated record: the
// record id is the stable file stem, because the physical file name also
// carries a date and a slug (and a dated note file holds several note
// sections). It is the address a client shows and the prefix a protected glob
// matches, not a promise that a file with exactly this name exists.
func RecordPath(projectID, kind, id string) string {
	return contextRecordPath(projectID, kind, id)
}
