package gitstore

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// maxErrorBytes caps sanitized stderr so a noisy git command cannot flood the
// caller with output.
const maxErrorBytes = 4096

var (
	urlUserInfoPattern = regexp.MustCompile(`((?:https?|ssh|git)://)[^/\s@]+@`)
	secretQueryPattern = regexp.MustCompile(`(?i)([?&](?:access_token|private_token|token|password|passwd)=)[^&\s"']+`)
	knownTokenPattern  = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_\-]{20,}|xox[abposr]-[A-Za-z0-9\-]{10,})\b`)
)

// CommandError reports a failed git invocation. Stderr is sanitized before it
// is stored, so remote userinfo, token query parameters, and well-known token
// shapes never reach logs or API responses.
type CommandError struct {
	Command  string
	Stderr   string
	ExitCode int
	Err      error
}

// Error implements error.
func (e *CommandError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("gitstore: git %s failed: %v", e.Command, e.Err)
	}
	return fmt.Sprintf("gitstore: git %s failed (exit %d): %s", e.Command, e.ExitCode, e.Stderr)
}

// Unwrap exposes the underlying exec error.
func (e *CommandError) Unwrap() error { return e.Err }

// ConflictError reports unmerged paths left by pull --rebase or an autostash
// restore. It unwraps to domain.ErrConflict so callers can branch on errors.Is.
type ConflictError struct {
	Operation string
	Paths     []string
	// Aborted reports that an in-progress rebase was aborted before the error
	// was returned, so the checkout is usable and needs no `git rebase --abort`.
	Aborted bool
}

// Error implements error.
func (e *ConflictError) Error() string {
	message := fmt.Sprintf("gitstore: %s stopped on a conflict", e.Operation)
	if len(e.Paths) > 0 {
		message += " in " + strings.Join(e.Paths, ", ")
	}
	if e.Aborted {
		message += " (rebase aborted; local commits and the autostash are restored)"
	}
	return message
}

// Unwrap maps the conflict to the domain sentinel.
func (e *ConflictError) Unwrap() error { return domain.ErrConflict }

func newCommandError(command, stderr string, err error) error {
	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	return &CommandError{
		Command:  command,
		Stderr:   sanitize(stderr),
		ExitCode: exitCode,
		Err:      err,
	}
}

// sanitize redacts credentials and caps length. Git stderr is free-form, so
// the function removes URL userinfo, secret query parameters, and common token
// prefixes rather than trying to parse git's error format.
func sanitize(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = urlUserInfoPattern.ReplaceAllString(text, "${1}***@")
	text = secretQueryPattern.ReplaceAllString(text, "${1}***")
	text = knownTokenPattern.ReplaceAllString(text, "***")
	text = strings.TrimSpace(text)
	if len(text) > maxErrorBytes {
		text = strings.ToValidUTF8(text[:maxErrorBytes], "")
		text = strings.TrimSpace(text) + "..."
	}
	return text
}
