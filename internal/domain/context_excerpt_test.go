package domain_test

import (
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestContextGrantsNormalizeAndValidate(t *testing.T) {
	// Arrange
	grants := domain.ContextGrants{
		SchemaVersion: 1,
		Grants: []domain.ContextGrant{{
			Project: " p ", Connection: " tracker ",
			Kinds: []domain.ItemKind{" Issue ", "issue", "COMMIT"},
		}},
	}

	// Act
	grants.Normalize()
	err := grants.Validate()

	// Assert
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	grant := grants.Grants[0]
	if grant.Project != "p" || grant.Connection != "tracker" || grant.MaxExcerpts != domain.MaxContextExcerpts {
		t.Fatalf("grant = %+v", grant)
	}
	if len(grant.Kinds) != 2 || grant.Kinds[0] != domain.ItemIssue || grant.Kinds[1] != domain.ItemCommit {
		t.Fatalf("kinds = %+v", grant.Kinds)
	}
}

func TestContextGrantsRejectInvalidDocuments(t *testing.T) {
	valid := domain.ContextGrant{Project: "p", Connection: "c", MaxExcerpts: 5}
	for name, grants := range map[string]domain.ContextGrants{
		"schema":      {SchemaVersion: 2, Grants: []domain.ContextGrant{valid}},
		"project":     {SchemaVersion: 1, Grants: []domain.ContextGrant{{Connection: "c", MaxExcerpts: 5}}},
		"connection":  {SchemaVersion: 1, Grants: []domain.ContextGrant{{Project: "p", MaxExcerpts: 5}}},
		"kind":        {SchemaVersion: 1, Grants: []domain.ContextGrant{{Project: "p", Connection: "c", Kinds: []domain.ItemKind{"bogus"}, MaxExcerpts: 5}}},
		"maxExcerpts": {SchemaVersion: 1, Grants: []domain.ContextGrant{{Project: "p", Connection: "c", MaxExcerpts: 6}}},
		"maxAgeDays":  {SchemaVersion: 1, Grants: []domain.ContextGrant{{Project: "p", Connection: "c", MaxAgeDays: -1, MaxExcerpts: 5}}},
		"duplicate":   {SchemaVersion: 1, Grants: []domain.ContextGrant{valid, valid}},
	} {
		t.Run(name, func(t *testing.T) {
			grants.Normalize()
			if err := grants.Validate(); err == nil {
				t.Fatalf("%+v accepted", grants)
			}
		})
	}
}

func TestContextGrantMatchAndAge(t *testing.T) {
	// Arrange
	grants := domain.ContextGrants{
		SchemaVersion: 1,
		Grants: []domain.ContextGrant{
			{Project: "p", Connection: "tracker", Kinds: []domain.ItemKind{domain.ItemIssue}, MaxExcerpts: 5},
			{Project: "p", Connection: "mail", MaxAgeDays: 2, MaxExcerpts: 5},
		},
	}
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

	// Assert: exact project and connection, kind filter.
	if grant, ok := grants.Match("p", "tracker", domain.ItemIssue); !ok || grant.Connection != "tracker" {
		t.Fatalf("issue grant = %+v, %v", grant, ok)
	}
	if _, ok := grants.Match("p", "tracker", domain.ItemCommit); ok {
		t.Fatal("commit matched an issue-only grant")
	}
	if _, ok := grants.Match("other", "tracker", domain.ItemIssue); ok {
		t.Fatal("grant matched a different project")
	}
	if _, ok := grants.Match("p", "other", domain.ItemIssue); ok {
		t.Fatal("grant matched a different connection")
	}

	// Assert: empty kinds is the documented wildcard.
	all, ok := grants.Match("p", "mail", domain.ItemMessage)
	if !ok {
		t.Fatal("wildcard kind grant did not match")
	}
	if !all.AllowsAge(now.AddDate(0, 0, -1), now) || all.AllowsAge(now.AddDate(0, 0, -3), now) {
		t.Fatal("maxAgeDays filter is wrong")
	}
	if !all.AllowsAge(time.Time{}, now) {
		t.Fatal("unknown time must not be filtered")
	}
	if noLimit := (domain.ContextGrant{}); !noLimit.AllowsAge(now.AddDate(-1, 0, 0), now) {
		t.Fatal("zero maxAgeDays must disable the filter")
	}
}
