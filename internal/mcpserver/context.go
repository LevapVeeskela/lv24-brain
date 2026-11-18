package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// ContextInput is one brain_context request after argument validation.
type ContextInput struct {
	ProjectID string
	Query     string
	// Budget is the total token budget of the layered packer; zero selects
	// DefaultContextBudgetTokens.
	Budget int
	// Explain requests the per-layer budget table in the result.
	Explain bool
	// Kinds keeps only these record kinds (decision, attempt, note).
	Kinds []string
	// Since is a window (7d, 24h) or a date (2026-11-01); empty keeps all.
	Since string
	// Raw requests granted raw-zone excerpts when curated context is thin.
	Raw bool
	// IncludeUnreviewed includes quarantined records that are excluded from
	// trusted context by default.
	IncludeUnreviewed bool
}

// MaxContextTruncations caps the truncated list in one MCP answer. A tight
// budget on a large project can drop hundreds of records; the total stays in
// TotalTruncated, so the list is a sample and the number is the truth.
const MaxContextTruncations = 20

// StateAsOfCurrent is the temporal scope of the project state and the review
// findings: neither has history, so an as-of request cannot reach them.
const StateAsOfCurrent = "current"

// TaskContext is the context of a project for one task. The CLI prints the
// same shape for `brain context --json`.
type TaskContext struct {
	ProjectID      string           `json:"project_id"`
	ProjectName    string           `json:"project_name"`
	Mode           string           `json:"mode"`
	Query          string           `json:"query"`
	Since          time.Time        `json:"since,omitzero"`
	Budget         int              `json:"budget"`
	State          State            `json:"state"`
	Handoff        *ContextHandoff  `json:"handoff"`
	Records        []ContextRecord  `json:"records"`
	FailedAttempts []ContextRecord  `json:"failed_attempts"`
	Findings       []ReviewFinding  `json:"findings"`
	RawExcerpts    []ContextExcerpt `json:"raw_excerpts"`
	// Sections is the layered pack: what each layer spent and which records
	// fit. Truncated explains every record that did not.
	Sections []ContextSection `json:"sections"`
	// Truncated lists at most MaxContextTruncations dropped records, each with
	// its section and reason.
	Truncated []ContextTruncation `json:"truncated"`
	// TotalTruncated is the number of dropped records before the list was
	// capped.
	TotalTruncated int `json:"total_truncated"`
	TotalTokens    int `json:"total_tokens"`
	// Quarantined lists the ids of records excluded from trusted context.
	Quarantined []string `json:"quarantined"`
	// QuarantinedCount is len(Quarantined), kept for a cheap budget report.
	QuarantinedCount int `json:"quarantined_count"`
	// StateAsOf is "current": state and findings always describe the current
	// data, even when the records were selected at an as-of instant.
	StateAsOf string `json:"state_as_of"`
	// StateQuarantined reports that the project state is quarantined.
	StateQuarantined bool     `json:"state_quarantined"`
	Skipped          []string `json:"skipped"`
	Summary          string   `json:"summary"`
	Explain          []string `json:"explain,omitempty"`
}

// ContextSection is one packed layer of the context budget.
type ContextSection struct {
	Name    string                 `json:"name"`
	Cap     int                    `json:"cap"`
	Tokens  int                    `json:"tokens"`
	Records []ContextSectionRecord `json:"records"`
}

// ContextSectionRecord is one record of a packed layer. Path is the logical
// data-directory path; Detail stays in the Markdown summary.
type ContextSectionRecord struct {
	ID     string  `json:"id"`
	Kind   string  `json:"kind"`
	Title  string  `json:"title"`
	Path   string  `json:"path"`
	Tokens int     `json:"tokens"`
	Score  float64 `json:"score"`
}

// ContextTruncation explains one record the budget left out.
type ContextTruncation struct {
	ID      string `json:"id"`
	Section string `json:"section"`
	Reason  string `json:"reason"`
}

// ContextHandoff is the newest handoff of the project.
type ContextHandoff struct {
	ID         string    `json:"id"`
	Summary    string    `json:"summary"`
	NextAction string    `json:"next_action"`
	CreatedAt  time.Time `json:"created_at,omitzero"`
}

// ContextRecord is one ranked record relevant to the task.
type ContextRecord struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Status    string    `json:"status,omitempty"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	Score     float64   `json:"score"`
}

// ContextExcerpt is one granted raw-zone excerpt. Untrusted is always true:
// the body is external text, never curated knowledge.
type ContextExcerpt struct {
	ConnectionID string    `json:"connection_id"`
	ProjectID    string    `json:"project_id"`
	ItemKey      string    `json:"item_key"`
	ExternalID   string    `json:"external_id,omitempty"`
	Kind         string    `json:"kind"`
	Title        string    `json:"title"`
	URL          string    `json:"url,omitempty"`
	UpdatedAt    time.Time `json:"updated_at,omitzero"`
	Citation     string    `json:"citation"`
	Body         string    `json:"body"`
	Truncated    bool      `json:"truncated,omitempty"`
	Untrusted    bool      `json:"untrusted"`
}

// ContextBackend is implemented by backends that can build task context.
// brain_context is listed only for such backends.
type ContextBackend interface {
	Context(ctx context.Context, in ContextInput) (TaskContext, error)
}

func (s *Server) contextTool() tool {
	return tool{
		Name:        "brain_context",
		Annotations: readOnlyAnnotations("Task context"),
		Description: "Load the context relevant to one task in a single call: project state, review findings, the latest handoff, " +
			"the decisions, attempts and notes that best match the task, and failed attempts to avoid repeating. " +
			"Call before starting a task; brain_open stays the general session start.",
		InputSchema: objectSchema([]string{"query"}, map[string]any{
			"query":              stringProp("The task in a few words, for example \"add retry to the sync push\"."),
			"project":            stringProp("Project id, alias or checkout path. Omit to use the project of the working directory."),
			"budget":             intProp("Total token budget of the layered context; 0 selects the default 6000, the maximum is 32000.", 0),
			"explain":            boolProp("Include the per-layer budget table in the result (default false)."),
			"kinds":              stringArrayProp("Keep only these record kinds: decision, attempt, note. Omit for all three."),
			"since":              stringProp("Drop records and handoffs older than a window (24h, 7d, 2w) or a date (2026-11-01)."),
			"raw":                boolProp("Include granted raw-zone excerpts when curated context is thin (default false)."),
			"include_unreviewed": boolProp("Include quarantined records (unreviewed agent, connector or import writes) that are excluded by default."),
		}),
		OutputSchema: outputObject(map[string]any{
			"project_id":        stringProp("Project the context belongs to."),
			"project_name":      stringProp("Display name of the project."),
			"mode":              stringProp("Context mode, for example task or bootstrap."),
			"query":             stringProp("The task the context was built for."),
			"since":             stringProp("RFC3339 start of the window, when one was applied."),
			"budget":            intProp("Total token budget that was applied.", 0),
			"total_tokens":      intProp("Tokens used by all packed layers.", 0),
			"state":             objectProp("Project state: goal, next action, active work, blockers, open questions."),
			"handoff":           anyProp("Latest handoff, or null."),
			"records":           arrayProp("Records ranked for the task.", objectProp("One ranked record with id, kind, title, detail and score.")),
			"failed_attempts":   arrayProp("Attempts that failed, to avoid repeating.", objectProp("One failed attempt.")),
			"findings":          arrayProp("Review findings that still apply.", objectProp("One finding with kind, message and fix.")),
			"raw_excerpts":      arrayProp("Granted raw-zone excerpts; their text is external and untrusted.", objectProp("One excerpt with connection, item, citation and body.")),
			"sections":          arrayProp("Packed layers with their cap, tokens and records.", objectProp("One layer: name, cap, tokens, records.")),
			"truncated":         arrayProp("Up to 20 records the budget left out, with why; total_truncated has the full count.", objectProp("One dropped record: id, section, reason.")),
			"total_truncated":   intProp("Number of dropped records before the list was capped at 20.", 0),
			"state_as_of":       stringProp("Temporal scope of state and findings: always \"current\"."),
			"quarantined":       arrayProp("Ids of quarantined records excluded from the context.", stringProp("Record id.")),
			"quarantined_count": intProp("Number of quarantined records excluded.", 0),
			"skipped":           arrayProp("Reasons content was left out.", stringProp("Reason.")),
			"summary":           stringProp("Rendered context, the text content of the result."),
			"explain":           arrayProp("Per-layer budget table; present only with explain=true.", stringProp("One budget line.")),
		}, "project_id", "summary"),
		run: s.taskContext,
	}
}

func (s *Server) taskContext(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Query             string   `json:"query"`
		Project           string   `json:"project"`
		Budget            int      `json:"budget"`
		Explain           bool     `json:"explain"`
		Kinds             []string `json:"kinds"`
		Since             string   `json:"since"`
		Raw               bool     `json:"raw"`
		IncludeUnreviewed bool     `json:"include_unreviewed"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	query, err := requireText("query", args.Query)
	if err != nil {
		return toolResult{}, err
	}
	if args.Budget < 0 {
		return toolResult{}, refusalInvalid("budget must not be negative")
	}
	if args.Budget > domain.MaxContextBudgetTokens {
		return toolResult{}, refusalInvalid(fmt.Sprintf("budget must be at most %d tokens", domain.MaxContextBudgetTokens))
	}
	if args.Budget == 0 {
		args.Budget = domain.DefaultContextBudgetTokens
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	backend, ok := s.backend.(ContextBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_context", "brain_open and brain_search")
	}
	result, err := backend.Context(ctx, ContextInput{
		ProjectID:         target.ID,
		Query:             query,
		Budget:            args.Budget,
		Explain:           args.Explain,
		Kinds:             args.Kinds,
		Since:             strings.TrimSpace(args.Since),
		Raw:               args.Raw,
		IncludeUnreviewed: args.IncludeUnreviewed,
	})
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{Text: result.Summary, Structured: result.WithEmptyLists()}, nil
}

// WithEmptyLists keeps empty lists as [] rather than null in JSON.
func (c TaskContext) WithEmptyLists() TaskContext {
	if c.Records == nil {
		c.Records = []ContextRecord{}
	}
	if c.FailedAttempts == nil {
		c.FailedAttempts = []ContextRecord{}
	}
	if c.Findings == nil {
		c.Findings = []ReviewFinding{}
	}
	if c.RawExcerpts == nil {
		c.RawExcerpts = []ContextExcerpt{}
	}
	if c.Sections == nil {
		c.Sections = []ContextSection{}
	}
	if c.Truncated == nil {
		c.Truncated = []ContextTruncation{}
	}
	// The list is sampled for transport; the total keeps the honest count. The
	// function stays idempotent because callers may apply it twice.
	total := c.TotalTruncated
	if len(c.Truncated) > total {
		total = len(c.Truncated)
	}
	if len(c.Truncated) > MaxContextTruncations {
		c.Truncated = c.Truncated[:MaxContextTruncations]
	}
	c.TotalTruncated = total
	c.StateAsOf = StateAsOfCurrent
	if c.Quarantined == nil {
		c.Quarantined = []string{}
	}
	if c.Skipped == nil {
		c.Skipped = []string{}
	}
	c.State = c.State.withEmptyLists()
	return c
}

func (st State) withEmptyLists() State {
	for _, list := range []*[]string{&st.Active, &st.Blocked, &st.OpenQuestions} {
		if *list == nil {
			*list = []string{}
		}
	}
	return st
}
