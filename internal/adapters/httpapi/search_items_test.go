package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestSearchKindParamsAcceptsItem(t *testing.T) {
	for _, name := range []string{"kind", "exclude_kind"} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/search?q=raw&"+name+"=item&"+name+"=note", nil)
			got, err := searchKindParams(r, name)
			if err != nil || !reflect.DeepEqual(got, []string{"item", "note"}) {
				t.Fatalf("%s=%v, %v", name, got, err)
			}
		})
	}
}

func TestSearchHandlerAcceptsItemWithoutNetwork(t *testing.T) {
	// Arrange: exercise the actual handler and access scope in process.
	store := NewMemoryStore()
	seedSearchKinds(t, store)
	api, err := New(testConfig(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)
	ctx := context.WithValue(context.Background(), principalContextKey{}, auth.Principal{User: domain.User{ID: "admin", Role: domain.RoleAdmin}})
	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		{"include", "kind=item", nil},
		{"exclude", "exclude_kind=item", []string{"decision", "handoff", "note"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/search?q=rollout&"+tc.query, nil).WithContext(ctx)
			response := httptest.NewRecorder()
			// Act.
			api.search(response, request)
			// Assert: item is valid in both filters and reaches the store.
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			got := searchKindsOf(t, response.Body.Bytes())
			if len(got) != len(tc.want) {
				t.Fatalf("kinds=%v want=%v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("kinds=%v want=%v", got, tc.want)
				}
			}
		})
	}
}

func TestOpenAPIItemAndConnectorRunContracts(t *testing.T) {
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Name   string `json:"name"`
					Schema struct {
						Enum []string `json:"enum"`
					} `json:"schema"`
				} `json:"parameters"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatal(err)
	}
	for _, parameter := range []string{"kind", "exclude_kind"} {
		found := false
		for _, p := range spec.Paths["/v1/search"].Get.Parameters {
			if p.Name == parameter {
				for _, v := range p.Schema.Enum {
					if v == "item" {
						found = true
					}
				}
			}
		}
		if !found {
			t.Errorf("%s enum missing item", parameter)
		}
	}
	run := spec.Components.Schemas["ConnectorRun"]
	var connectionID struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(run.Properties["connectionId"], &connectionID); err != nil || connectionID.Type != "string" {
		t.Errorf("connectionId: %s %v", run.Properties["connectionId"], err)
	}
	var stats struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(run.Properties["stats"], &stats); err != nil || stats.Type != "object" {
		t.Errorf("stats: %s %v", run.Properties["stats"], err)
	}
	for _, field := range []string{"seen", "upserted", "unchanged", "deleted", "skipped", "batches", "truncated"} {
		if _, ok := stats.Properties[field]; !ok {
			t.Errorf("missing stats.%s", field)
		}
	}
	for _, field := range run.Required {
		if field == "connectionId" || field == "stats" {
			t.Errorf("%s must be optional", field)
		}
	}
	for _, pair := range [][2]string{{"ConnectorRun", "source"}, {"ConnectorRunRequest", "name"}} {
		var property struct {
			Type        string   `json:"type"`
			Enum        []string `json:"enum"`
			Description string   `json:"description"`
		}
		if err := json.Unmarshal(spec.Components.Schemas[pair[0]].Properties[pair[1]], &property); err != nil {
			t.Fatal(err)
		}
		if property.Type != "string" || len(property.Enum) != 0 || property.Description == "" {
			t.Errorf("%v: %+v", pair, property)
		}
	}
}
