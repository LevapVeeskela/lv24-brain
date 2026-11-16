package config

import (
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// GraphConfig is the `graph` block of brain.yaml: which code-graph engine
// answers `brain code`. It is disabled unless configured, because indexing a
// checkout is an explicit decision by the workspace owner.
type GraphConfig struct {
	// Engine selects the code-graph engine: "off" (default), "native" (the
	// built-in Go indexer) or "codegraph" (an external read-only index).
	Engine string `yaml:"engine,omitempty"`
	// Path optionally overrides the index location. For "native" it is the
	// database file or a directory holding it; for "codegraph" it is the
	// codegraph.db file or a directory containing .codegraph/codegraph.db.
	// A relative path is resolved against the project checkout.
	Path string `yaml:"path,omitempty"`
}

// IsZero lets yaml omit a graph block that was never configured.
func (g GraphConfig) IsZero() bool {
	return strings.TrimSpace(g.Engine) == "" && strings.TrimSpace(g.Path) == ""
}

// EffectiveEngine resolves an omitted engine to "off". There is no implicit
// engine: a workspace must opt in.
func (g GraphConfig) EffectiveEngine() string {
	engine := strings.ToLower(strings.TrimSpace(g.Engine))
	if engine == "" {
		return ports.GraphEngineOff
	}
	return engine
}

// Validate rejects an unknown engine and names the supported values.
func (g GraphConfig) Validate() error {
	switch g.EffectiveEngine() {
	case ports.GraphEngineOff, ports.GraphEngineCodeGraph, ports.GraphEngineNative:
		return nil
	default:
		return domain.NewValidationError("config.graph.engine",
			fmt.Sprintf("unknown engine %q; use %q, %q or %q",
				g.Engine, ports.GraphEngineNative, ports.GraphEngineCodeGraph, ports.GraphEngineOff))
	}
}
