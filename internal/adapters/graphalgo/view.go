package graphalgo

import (
	"fmt"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// LowConfidence is the edge confidence below which a hop is disclosed as
// heuristic instead of presented as a fact. Both graph adapters use it, so path
// and impact answers agree.
const LowConfidence = 0.7

// RiskTier ranks the blast radius of a diff. A deleted file is always high: a
// missing definition breaks its callers at compile time. The thresholds are
// shared by both engines so the same diff gets the same tier.
func RiskTier(impact, changed, deleted int) string {
	switch {
	case deleted > 0 || impact >= 50 || changed >= 25:
		return ports.GraphRiskHigh
	case impact >= 10 || changed >= 5:
		return ports.GraphRiskMedium
	default:
		return ports.GraphRiskLow
	}
}

// PathView assembles the port view of a found path. symbols must hold every
// step id; a missing symbol is rendered as an id-only stub rather than dropped,
// because dropping it would silently shorten the chain.
func PathView(from, to ports.GraphSymbol, steps []PathStep, symbols map[string]ports.GraphSymbol, directed bool, maxNodes int) ports.GraphPath {
	view := ports.GraphPath{From: from, To: to, Found: true, Directed: directed}
	if maxNodes > 0 && len(steps) > maxNodes {
		steps = steps[:maxNodes]
		view.Notes = append(view.Notes, fmt.Sprintf("path truncated at %d nodes", maxNodes))
	}
	confidence := 1.0
	weak := 0
	view.Steps = make([]ports.GraphPathStep, 0, len(steps))
	for index, step := range steps {
		symbol, ok := symbols[step.ID]
		if !ok {
			symbol = ports.GraphSymbol{ID: step.ID}
		}
		if index > 0 {
			if step.Confidence < confidence {
				confidence = step.Confidence
			}
			if step.Confidence < LowConfidence {
				weak++
			}
		}
		view.Steps = append(view.Steps, ports.GraphPathStep{
			GraphSymbol: symbol, Edge: step.Edge, Line: step.Line,
			Reverse: step.Reverse, Confidence: step.Confidence,
		})
	}
	view.Hops = len(steps) - 1
	view.Confidence = confidence
	if weak > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d hop(s) resolve below %.1f confidence", weak, LowConfidence))
	}
	return view
}
