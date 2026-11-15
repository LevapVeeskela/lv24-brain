package config

import (
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// GraphConfig is the `graph` block of brain.yaml: which code-graph engine
// answers `brain code`. It is disabled unless configured, because reading an
// external index needs an explicit path decision by the workspace owner.
type GraphConfig struct {
	// Engine selects the code-graph engine: "off" (default) or "codegraph".
	// The native Go indexer is a later work package.
	Engine string `yaml:"engine,omitempty"`
	// Path optionally overrides the index location. It is either the
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
	case ports.GraphEngineOff, ports.GraphEngineCodeGraph:
		return nil
	case "native":
		return domain.NewValidationError("config.graph.engine",
			fmt.Sprintf("%q is not available yet; use %q or %q", "native", ports.GraphEngineCodeGraph, ports.GraphEngineOff))
	default:
		return domain.NewValidationError("config.graph.engine",
			fmt.Sprintf("unknown engine %q; use %q or %q", g.Engine, ports.GraphEngineCodeGraph, ports.GraphEngineOff))
	}
}
