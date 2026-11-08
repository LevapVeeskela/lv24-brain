package app_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/app"
)

func TestConcurrentSetStateKeepsBothFields(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	registerScopeProjects(t, dataDir)
	a, _ := newScopeFixture(t, dataDir)
	const rounds = 50

	var wg sync.WaitGroup
	errs := make(chan error, 2*rounds)
	for _, field := range []string{"goal", "next"} {
		wg.Add(1)
		go func(field string) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				value := fmt.Sprintf("%s-%d", field, i)
				input := app.StateInput{ProjectID: "p1"}
				if field == "goal" {
					input.Goal = &value
				} else {
					input.NextAction = &value
				}
				if _, err := a.SetState(ctx, input); err != nil {
					errs <- err
				}
			}
		}(field)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("SetState: %v", err)
	}

	_, state, err := a.GetState(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("goal-%d", rounds-1); state.Goal != want {
		t.Fatalf("goal = %q, want %q (lost update)", state.Goal, want)
	}
	if want := fmt.Sprintf("next-%d", rounds-1); state.NextAction != want {
		t.Fatalf("next action = %q, want %q (lost update)", state.NextAction, want)
	}
}
