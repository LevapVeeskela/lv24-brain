package serverstore

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestBuildTSQuery(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantExpr string
		wantArgs []any
	}{
		{
			name:     "plain word gets stem prefix",
			text:     "decisions",
			wantExpr: "(((websearch_to_tsquery('simple', $9::text) || to_tsquery('simple', $10::text))))",
			wantArgs: []any{"decisions", "'decision':*"},
		},
		{
			name:     "russian word gets stem prefix",
			text:     "решений",
			wantExpr: "(((websearch_to_tsquery('simple', $9::text) || to_tsquery('simple', $10::text))))",
			wantArgs: []any{"решений", "'решен':*"},
		},
		{
			name:     "short word stays exact",
			text:     "db",
			wantExpr: "((websearch_to_tsquery('simple', $9::text)))",
			wantArgs: []any{"db"},
		},
		{
			name: "phrase and exclusion stay websearch",
			text: `"fat rats" -docker`,
			wantExpr: "((websearch_to_tsquery('simple', $9::text) && " +
				"websearch_to_tsquery('simple', $10::text)))",
			wantArgs: []any{`"fat rats"`, "-docker"},
		},
		{
			name: "OR separates alternatives",
			text: "deploy OR release",
			wantExpr: "(((websearch_to_tsquery('simple', $9::text) || to_tsquery('simple', $10::text))) || " +
				"((websearch_to_tsquery('simple', $11::text) || to_tsquery('simple', $12::text))))",
			wantArgs: []any{"deploy", "'deploy':*", "release", "'release':*"},
		},
		{
			name:     "dangling OR is dropped",
			text:     "or db or",
			wantExpr: "((websearch_to_tsquery('simple', $9::text)))",
			wantArgs: []any{"db"},
		},
		{
			name:     "punctuation keeps the whole text",
			text:     "100%",
			wantExpr: "((websearch_to_tsquery('simple', $9::text)))",
			wantArgs: []any{"100%"},
		},
		{
			name:     "injection attempt is a bound value",
			text:     "x'); DROP TABLE notes; --",
			wantExpr: "",
			wantArgs: []any{"x');", "DROP", "'drop':*", "TABLE", "'table':*", "notes;", "'note':*", "--"},
		},
		{
			name:     "unclosed quote runs to the end",
			text:     `alpha "beta gamma`,
			wantExpr: "",
			wantArgs: []any{"alpha", "'alpha':*", `"beta gamma`},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expr, args := buildTSQuery(test.text, searchFirstTSParam)
			if test.wantExpr != "" && expr != test.wantExpr {
				t.Fatalf("expr = %s\nwant   %s", expr, test.wantExpr)
			}
			if !reflect.DeepEqual(args, test.wantArgs) {
				t.Fatalf("args = %#v, want %#v", args, test.wantArgs)
			}
			for _, arg := range args {
				if strings.Contains(expr, arg.(string)) {
					t.Fatalf("expr %q embeds user text %q", expr, arg)
				}
			}
		})
	}
}

func TestBuildTSQueryLongQueryFallsBackToWholeText(t *testing.T) {
	text := strings.Repeat("word ", maxTSClauses+1)
	expr, args := buildTSQuery(text, searchFirstTSParam)
	if expr != "websearch_to_tsquery('simple', $9::text)" || len(args) != 1 || args[0] != text {
		t.Fatalf("buildTSQuery(long) = %s %v", expr, args)
	}
}

func TestBuildTSQueryEmptyKeepsWholeText(t *testing.T) {
	expr, args := buildTSQuery("   ", searchFirstTSParam)
	if expr != "websearch_to_tsquery('simple', $9::text)" || len(args) != 1 {
		t.Fatalf("buildTSQuery(empty) = %s %v", expr, args)
	}
}

func TestSearchStatementReplacesMarker(t *testing.T) {
	statement := searchStatement("EXPR")
	if strings.Contains(statement, searchTSQueryMarker) || !strings.Contains(statement, "SELECT EXPR AS tsq") {
		t.Fatalf("statement = %s", statement)
	}
}

func TestStoreSearchWordFormsAndSince(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	project := mustProject(t, ctx, store, "forms")
	notes := []domain.Note{
		{ProjectID: project.ID, Title: "Storage", Body: "one decision about storage"},
		{ProjectID: project.ID, Title: "Журнал", Body: "принято решение по хранилищу"},
		{ProjectID: project.ID, Title: "Deploys", Body: "the server was deployed"},
		{ProjectID: project.ID, Title: "Docker", Body: "deploy with docker"},
	}
	for _, note := range notes {
		if _, _, err := store.CreateNote(ctx, note); err != nil {
			t.Fatalf("create note: %v", err)
		}
	}

	searchTitles := func(query ports.Query) []string {
		t.Helper()
		query.ProjectID = project.ID
		hits, err := store.Search(ctx, query)
		if err != nil {
			t.Fatalf("Search(%q): %v", query.Text, err)
		}
		titles := make([]string, 0, len(hits))
		for _, hit := range hits {
			titles = append(titles, hit.Title)
		}
		return titles
	}
	assertTitles := func(query ports.Query, want ...string) {
		t.Helper()
		got := searchTitles(query)
		if len(got) != len(want) {
			t.Fatalf("Search(%q) = %v, want %v", query.Text, got, want)
		}
		for _, title := range want {
			found := false
			for _, candidate := range got {
				found = found || candidate == title
			}
			if !found {
				t.Fatalf("Search(%q) = %v, want %v", query.Text, got, want)
			}
		}
	}

	assertTitles(ports.Query{Text: "decisions"}, "Storage")
	assertTitles(ports.Query{Text: "решений"}, "Журнал")
	assertTitles(ports.Query{Text: "deploying"}, "Deploys", "Docker")
	assertTitles(ports.Query{Text: "deploying -docker"}, "Deploys")
	assertTitles(ports.Query{Text: `"one decision"`}, "Storage")
	assertTitles(ports.Query{Text: "decisions OR хранилищу"}, "Storage", "Журнал")
	assertTitles(ports.Query{Text: "decision", Since: time.Now().Add(time.Hour)})
	assertTitles(ports.Query{Text: "decision", Since: time.Now().Add(-time.Hour)}, "Storage")
}
