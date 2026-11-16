package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/adapters/codegraph"
	"github.com/levapveeskela/lv24-brain/internal/adapters/codeindex"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// GraphOptions selects the code-graph engine of one command.
type GraphOptions struct {
	// Engine is "native", "codegraph" or "off".
	Engine string
	// Root is the checkout the index describes and source is read from.
	Root string
	// Project labels answers.
	Project string
	// DBPath overrides the resolved database path.
	DBPath string
	// Create lets a native Open materialize an empty store.
	Create bool
	// Tags are extra Go build tags for the native extractor.
	Tags []string
	// Exclude lists repository paths the native scanner skips.
	Exclude []string
}

// GraphRuntime is one open code-graph engine behind ports.GraphIndex.
type GraphRuntime struct {
	// Index is the read/query port. Both engines implement it.
	Index ports.GraphIndex
	// Native is the writable native store, nil for external engines.
	Native *codeindex.Index
}

// Close releases the engine.
func (g *GraphRuntime) Close() error {
	if g == nil || g.Index == nil {
		return nil
	}
	if closer, ok := g.Index.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// OpenGraph constructs the engine selected by opts. Native and codegraph share
// ports.GraphIndex; only the native engine can index.
func OpenGraph(ctx context.Context, opts GraphOptions) (*GraphRuntime, error) {
	engine := strings.ToLower(strings.TrimSpace(opts.Engine))
	switch engine {
	case "", ports.GraphEngineOff:
		return nil, domain.NewValidationError("graph.engine", "is off; set graph.engine or pass --engine")
	case ports.GraphEngineNative:
		index, err := codeindex.Open(ctx, codeindex.Options{
			Root:    opts.Root,
			DBPath:  opts.DBPath,
			Project: opts.Project,
			Tags:    opts.Tags,
			Exclude: opts.Exclude,
			Create:  opts.Create,
		})
		if err != nil {
			return nil, err
		}
		return &GraphRuntime{Index: index, Native: index}, nil
	case ports.GraphEngineCodeGraph:
		index, err := codegraph.Open(ctx, codegraph.Options{
			DBPath:  opts.DBPath,
			Root:    opts.Root,
			Project: opts.Project,
		})
		if err != nil {
			return nil, err
		}
		return &GraphRuntime{Index: index}, nil
	default:
		return nil, domain.NewValidationError("graph.engine",
			fmt.Sprintf("unknown engine %q; use %q or %q", opts.Engine, ports.GraphEngineNative, ports.GraphEngineCodeGraph))
	}
}

// GraphDBPath resolves the index file of one engine. An explicit override or
// graph.path wins; a native path may name the database file or a directory
// that receives the default file name. An empty native path resolves to
// <data-dir>/index/graph/<project>-<pathhash>.db.
func GraphDBPath(cfg config.BrainConfig, engine, root, project, override string) (string, error) {
	engine = strings.ToLower(strings.TrimSpace(engine))
	candidate := strings.TrimSpace(override)
	if candidate == "" {
		candidate = strings.TrimSpace(cfg.Graph.Path)
	}
	if !filepath.IsAbs(candidate) && candidate != "" {
		candidate = filepath.Join(root, candidate)
	}
	switch engine {
	case ports.GraphEngineNative:
		defaultPath, err := codeindex.DefaultDBPath(cfg.DataDir, project, root)
		if err != nil {
			return "", err
		}
		if candidate == "" {
			return defaultPath, nil
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return filepath.Join(candidate, filepath.Base(defaultPath)), nil
		}
		if strings.EqualFold(filepath.Ext(candidate), ".db") {
			return candidate, nil
		}
		return filepath.Join(candidate, filepath.Base(defaultPath)), nil
	case ports.GraphEngineCodeGraph:
		if candidate == "" {
			return filepath.Join(root, filepath.FromSlash(codegraph.DefaultDBRelPath)), nil
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return filepath.Join(candidate, filepath.FromSlash(codegraph.DefaultDBRelPath)), nil
		}
		if strings.EqualFold(filepath.Ext(candidate), ".db") {
			return candidate, nil
		}
		return filepath.Join(candidate, filepath.FromSlash(codegraph.DefaultDBRelPath)), nil
	default:
		return "", domain.NewValidationError("graph.engine", "is not a readable engine")
	}
}
