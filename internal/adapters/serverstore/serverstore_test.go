package serverstore

import (
	"errors"
	"reflect"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/adapters/postgres"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestNewRequiresPostgresStore(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) must fail")
	}
}

func TestMapErrorTranslatesRevisionConflict(t *testing.T) {
	source := &postgres.RevisionConflictError{
		Entity: "notes", ID: "n1", Expected: 2, Current: 5,
	}
	mapped := mapError(source)
	var conflict *httpapi.ConflictError
	if !errors.As(mapped, &conflict) {
		t.Fatalf("mapped error = %T, want *httpapi.ConflictError", mapped)
	}
	if conflict.Entity != "notes" || conflict.ID != "n1" || conflict.Expected != 2 || conflict.Current != 5 {
		t.Fatalf("conflict = %+v", conflict)
	}
	if !errors.Is(mapped, domain.ErrConflict) {
		t.Fatal("mapped conflict must unwrap to domain.ErrConflict")
	}
}

func TestMapErrorPassesThroughOtherErrors(t *testing.T) {
	if mapError(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	if err := mapError(domain.ErrNotFound); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("passthrough = %v", err)
	}
}

func TestEscapeLike(t *testing.T) {
	tests := map[string]string{
		"plain":   "plain",
		"100%":    "100!%",
		"a_b":     "a!_b",
		"bang!":   "bang!!",
		"!%_mix":  "!!!%!_mix",
		"":        "",
		"café_%!": "café!_!%!!",
	}
	for input, want := range tests {
		if got := escapeLike(input); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestOptionalArrayBindsEmptyFilterAsNull(t *testing.T) {
	if got := optionalArray(nil); got != nil {
		t.Fatalf("nil filter = %#v, want nil", got)
	}
	if got := optionalArray([]string{}); got != nil {
		t.Fatalf("empty filter = %#v, want nil", got)
	}
	if got := optionalArray([]string{"note"}); !reflect.DeepEqual(got, []string{"note"}) {
		t.Fatalf("filter = %#v", got)
	}
}
