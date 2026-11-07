// Package integrations holds agent integration templates that ship inside the
// brain binary.
package integrations

import _ "embed"

// BrainHandoffPlugin is the OpenCode session-end plugin template. It contains
// the __BRAIN_BIN__ and __BRAIN_CONFIG__ placeholders, which "brain agents sync
// --hooks" replaces with the running binary and the loaded config path.
//
//go:embed opencode/brain-handoff.js
var BrainHandoffPlugin string
