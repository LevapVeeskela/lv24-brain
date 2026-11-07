package app

import (
	"context"
	"time"
)

// SetCommitsTouchingForTest replaces the git query behind the
// decision-code-changed heuristic and returns a function that restores it.
func SetCommitsTouchingForTest(fn func(context.Context, string, time.Time, []string) ([]string, error)) func() {
	previous := commitsTouching
	commitsTouching = fn
	return func() { commitsTouching = previous }
}
