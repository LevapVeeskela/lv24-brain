package fsstore

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestUpdateStateConcurrentFieldsKeepBothWrites(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	ctx := context.Background()
	const rounds = 50

	var wg sync.WaitGroup
	errs := make(chan error, 2*rounds)
	setField := func(apply func(*domain.State, string), prefix string) {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			value := fmt.Sprintf("%s-%d", prefix, i)
			_, err := store.UpdateState(ctx, project.ID, func(state *domain.State) error {
				apply(state, value)
				return nil
			})
			if err != nil {
				errs <- err
			}
		}
	}
	wg.Add(2)
	go setField(func(s *domain.State, v string) { s.Goal = v }, "goal")
	go setField(func(s *domain.State, v string) { s.NextAction = v }, "next")
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("UpdateState: %v", err)
	}

	state, err := store.GetState(ctx, project.ID)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if want := fmt.Sprintf("goal-%d", rounds-1); state.Goal != want {
		t.Fatalf("goal = %q, want %q (lost update)", state.Goal, want)
	}
	if want := fmt.Sprintf("next-%d", rounds-1); state.NextAction != want {
		t.Fatalf("next action = %q, want %q (lost update)", state.NextAction, want)
	}
}

func TestUpdateStateStartsFromEmptyStateWhenMissing(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)

	state, err := store.UpdateState(context.Background(), project.ID, func(s *domain.State) error {
		if s.ProjectID != project.ID {
			return fmt.Errorf("ProjectID = %q, want %q", s.ProjectID, project.ID)
		}
		s.Goal = "first"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	if state.Goal != "first" || state.ProjectID != project.ID {
		t.Fatalf("state = %+v", state)
	}
}

func TestUpdateStateChangeErrorLeavesStateUnchanged(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	ctx := context.Background()
	if _, err := store.UpdateState(ctx, project.ID, func(s *domain.State) error {
		s.Goal = "kept"
		return nil
	}); err != nil {
		t.Fatalf("seed UpdateState: %v", err)
	}

	wantErr := fmt.Errorf("rejected")
	if _, err := store.UpdateState(ctx, project.ID, func(s *domain.State) error {
		s.Goal = "discarded"
		return wantErr
	}); err != wantErr {
		t.Fatalf("UpdateState err = %v, want %v", err, wantErr)
	}
	state, err := store.GetState(ctx, project.ID)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state.Goal != "kept" {
		t.Fatalf("goal = %q, want kept", state.Goal)
	}
}
