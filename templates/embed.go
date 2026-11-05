// Package templates embeds the generic Markdown templates used for brain
// records. Instance data never lives in this repository.
package templates

import "embed"

// FS contains state.md, decision.md, attempt.md, handoff.md and AGENTS.md.
//
//go:embed *.md
var FS embed.FS
