package domain

// Token budgets of the layered context packer. They are shared by the
// configuration, the use cases and the MCP surface so one number means the
// same thing everywhere.
const (
	// DefaultContextBudgetTokens is the total budget of `brain context` and
	// `brain_context` when the caller gives none.
	DefaultContextBudgetTokens = 6000
	// MaxContextBudgetTokens is the largest budget a caller may request. A
	// larger context stops being a curated answer and becomes a dump.
	MaxContextBudgetTokens = 32000
)
