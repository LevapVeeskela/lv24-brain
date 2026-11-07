package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/gitinfo"
)

const (
	// sessionTimeout bounds a session hook so it never stalls the agent.
	sessionTimeout = 10 * time.Second
	// sessionHandoffGap skips an automatic handoff when one was written this
	// recently (by the agent itself or another hook).
	sessionHandoffGap = 30 * time.Minute
	// sessionSummaryRunes bounds an automatic handoff summary.
	sessionSummaryRunes = 600
	// sessionContextBudget bounds the context injected at session start.
	sessionContextBudget = 6000
	autoHandoffPrefix    = "[auto] "
)

// hookInput is the subset of the JSON a coding agent passes to a session hook
// on stdin (Claude Code: SessionStart / SessionEnd; Codex: SessionStart /
// Stop, which carries the last assistant message).
type hookInput struct {
	Cwd                  string `json:"cwd"`
	TranscriptPath       string `json:"transcript_path"`
	Source               string `json:"source"`
	Reason               string `json:"reason"`
	LastAssistantMessage string `json:"last_assistant_message"`
	SessionID            string `json:"session_id"`
	HookEventName        string `json:"hook_event_name"`
}

// jsonHook reports hook protocols that pass JSON on stdin and expect the
// Claude-style hookSpecificOutput on stdout (Claude Code and Codex share it).
func jsonHook(hook string) bool {
	return hook == "claude" || hook == "codex"
}

type sessionOptions struct {
	hook    string
	cwd     string
	cfgPath string
	input   hookInput
}

// runSession implements `brain session start|end`, the handlers behind agent
// session hooks. In hook mode it never fails the agent: problems go to stderr
// and the exit code is 0; an unregistered directory is silently ignored.
func runSession(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "start" && args[0] != "end") {
		fmt.Fprintln(stderr, "usage: brain session start|end [--hook claude|codex|opencode] [--cwd <dir>]")
		return 2
	}
	action := args[0]
	fs := newFlagSet("session "+action, stderr)
	hook := fs.String("hook", "", "agent hook protocol: claude or codex (JSON on stdin), or opencode")
	cwd := fs.String("cwd", "", "project directory (default: from the hook input, else the working directory)")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain session %s: unexpected arguments: %s\n", action, strings.Join(fs.Args(), " "))
		return 2
	}
	switch *hook {
	case "", "claude", "codex", "opencode":
	default:
		fmt.Fprintf(stderr, "brain session %s: unknown --hook %q (want claude, codex or opencode)\n", action, *hook)
		return 2
	}
	opts := sessionOptions{hook: *hook, cwd: strings.TrimSpace(*cwd), cfgPath: *cfgPath}
	if jsonHook(opts.hook) {
		opts.input = readHookInput(stdin)
		if opts.cwd == "" {
			opts.cwd = strings.TrimSpace(opts.input.Cwd)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
	defer cancel()
	var err error
	switch {
	case action == "start":
		if opts.hook == "codex" {
			flushStaleCodexCheckpoints(ctx, opts)
		}
		err = sessionStart(ctx, opts, stdout)
	case opts.hook == "codex":
		err = codexSessionEnd(ctx, opts, stdout)
	default:
		err = sessionEnd(ctx, opts, stdout)
	}
	if err == nil {
		return 0
	}
	code := reportError(stderr, "session "+action, err)
	if opts.hook != "" {
		return 0
	}
	return code
}

func readHookInput(stdin io.Reader) hookInput {
	var input hookInput
	if stdin == nil {
		return input
	}
	data, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return input
	}
	_ = json.Unmarshal(data, &input)
	return input
}

// errNotRegistered marks a directory that belongs to no registered project;
// hooks stay silent for it.
var errNotRegistered = errors.New("directory is not a registered project")

func openSessionTarget(ctx context.Context, opts sessionOptions) (*bootstrap.Runtime, app.Resolved, error) {
	cfg, err := loadRequiredConfig(opts.cfgPath)
	if err != nil {
		return nil, app.Resolved{}, err
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return nil, app.Resolved{}, err
	}
	dir := opts.cwd
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			_ = runtime.Close()
			return nil, app.Resolved{}, err
		}
	}
	target, err := runtime.App.ResolveSelector(ctx, dir)
	if _, ok := app.AsWorkspace(err); ok {
		// A workspace root: the runtime stays open for the overview; the
		// caller closes it.
		return runtime, app.Resolved{}, err
	}
	if err != nil {
		_ = runtime.Close()
		return nil, app.Resolved{}, errNotRegistered
	}
	return runtime, target, nil
}

func sessionStart(ctx context.Context, opts sessionOptions, stdout io.Writer) error {
	runtime, target, err := openSessionTarget(ctx, opts)
	if workspace, ok := app.AsWorkspace(err); ok {
		defer func() { _ = runtime.Close() }()
		text, err := runtime.App.WorkspaceOverview(ctx, workspace)
		if err != nil {
			return err
		}
		return writeStartContext(opts, stdout, text)
	}
	if errors.Is(err, errNotRegistered) && opts.hook != "" {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = runtime.Close() }()
	text, err := sessionContext(ctx, runtime.App, target)
	if err != nil {
		return err
	}
	return writeStartContext(opts, stdout, text)
}

// writeStartContext prints session-start context in the hook's format.
func writeStartContext(opts sessionOptions, stdout io.Writer, text string) error {
	if !jsonHook(opts.hook) {
		_, err := io.WriteString(stdout, text)
		return err
	}
	return json.NewEncoder(stdout).Encode(claudeStartOutput{HookSpecificOutput: claudeHookContext{
		HookEventName:     "SessionStart",
		AdditionalContext: text,
	}})
}

type claudeStartOutput struct {
	HookSpecificOutput claudeHookContext `json:"hookSpecificOutput"`
}

type claudeHookContext struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// sessionContext is the curated project context plus what changed since the
// latest handoff.
func sessionContext(ctx context.Context, a *app.App, target app.Resolved) (string, error) {
	summary, err := a.Bootstrap(ctx, app.BootstrapOptions{ProjectID: target.Project.ID, Budget: sessionContextBudget})
	if err != nil {
		return "", err
	}
	catchup, err := a.Catchup(ctx, app.CatchupOptions{ProjectID: target.Project.ID, Limit: 10})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Company Brain: %s (from `brain session start`)\n\n", target.Project.ID)
	b.WriteString(strings.TrimSpace(summary.Summary))
	b.WriteString("\n\n## Since the latest handoff\n")
	printCatchup(&b, catchup)
	if findings, err := a.Findings(ctx, target.Project.ID); err == nil && len(findings) > 0 {
		fmt.Fprintf(&b, "\n%d review findings (stale or contradictory knowledge): run `brain review` and confirm or fix them.\n", len(findings))
	}
	b.WriteString("\nKeep the brain current: `brain state set` when the goal or next step changes; " +
		"`brain note` / `brain decision` / `brain attempt` for findings; `brain handoff` at the end.\n")
	return b.String(), nil
}

func sessionEnd(ctx context.Context, opts sessionOptions, stdout io.Writer) error {
	runtime, target, err := openSessionTarget(ctx, opts)
	if _, ok := app.AsWorkspace(err); ok {
		// No single project to hand off from a workspace root.
		_ = runtime.Close()
		return nil
	}
	if errors.Is(err, errNotRegistered) && opts.hook != "" {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = runtime.Close() }()
	a := runtime.App
	_, _, handoffs, err := a.RecentRecords(ctx, target.Project.ID, 1)
	if err != nil {
		return err
	}
	if len(handoffs) > 0 && time.Since(handoffs[0].CreatedAt) < sessionHandoffGap {
		return nil
	}
	summary, err := sessionSummary(ctx, a, target, opts)
	if err != nil || summary == "" {
		return err
	}
	_, state, err := a.GetState(ctx, target.Project.ID)
	if err != nil {
		return err
	}
	result, err := a.Handoff(ctx, app.HandoffInput{
		ProjectID:  target.Project.ID,
		Summary:    autoHandoffPrefix + truncateRunes(summary, sessionSummaryRunes),
		GitContext: gitinfo.Read(ctx, gitDir(target)).Context(),
		NextAction: state.NextAction,
	})
	if err != nil {
		return err
	}
	if opts.hook == "" {
		fmt.Fprintf(stdout, "handoff %s\n", result.Handoff.ID)
	}
	return nil
}

// sessionSummary picks the handoff text: the agent's last message when a
// transcript is available, else a digest of what was recorded since the
// latest handoff. Empty means there is nothing worth a handoff.
func sessionSummary(ctx context.Context, a *app.App, target app.Resolved, opts sessionOptions) (string, error) {
	if text := strings.Join(strings.Fields(opts.input.LastAssistantMessage), " "); text != "" {
		return text, nil
	}
	if opts.hook == "claude" && opts.input.TranscriptPath != "" {
		if text := lastAssistantText(opts.input.TranscriptPath); text != "" {
			return text, nil
		}
	}
	catchup, err := a.Catchup(ctx, app.CatchupOptions{ProjectID: target.Project.ID, Limit: 5})
	if err != nil || catchup.Empty() {
		return "", err
	}
	return catchupDigest(catchup), nil
}

func catchupDigest(result app.CatchupResult) string {
	var parts []string
	add := func(label string, count int, latest string) {
		if count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s (latest: %s)", count, label, app.SummarizeLine(latest, 80)))
		}
	}
	if len(result.Notes) > 0 {
		add("notes", len(result.Notes), valueOr(result.Notes[0].Title, result.Notes[0].Body))
	}
	if len(result.Decisions) > 0 {
		add("decisions", len(result.Decisions), result.Decisions[0].Title)
	}
	if len(result.Attempts) > 0 {
		add("attempts", len(result.Attempts), result.Attempts[0].Title)
	}
	if result.StateChanged {
		parts = append(parts, "state updated")
	}
	if len(result.Journal) > 0 {
		add("activity", len(result.Journal), formatJournalEntry(result.Journal[0]))
	}
	return "session activity: " + strings.Join(parts, "; ")
}

// lastAssistantText returns the last text an assistant wrote in a Claude Code
// JSONL transcript, whitespace-collapsed. Unreadable input yields "".
func lastAssistantText(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReader(file)
	last := ""
	for {
		line, err := reader.ReadBytes('\n')
		if text := assistantText(line); text != "" {
			last = text
		}
		if err != nil {
			return last
		}
	}
}

func assistantText(line []byte) string {
	if !bytes.Contains(line, []byte(`"assistant"`)) {
		return ""
	}
	var entry struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "assistant" {
		return ""
	}
	var texts []string
	for _, part := range entry.Message.Content {
		if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(strings.Fields(strings.Join(texts, " ")), " ")
}

func truncateRunes(text string, limit int) string {
	if runes := []rune(text); len(runes) > limit {
		return strings.TrimSpace(string(runes[:limit])) + "…"
	}
	return text
}
