package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func validConnection() Connection {
	return Connection{
		ID:       "github-acme",
		Provider: "github",
		Account:  "acme-bot",
		BaseURL:  "https://api.github.com",
		Projects: []string{"lv24"},
		Schedule: "@every 15m",
		Settings: map[string]string{"repos": "acme/widgets"},
	}
}

func testDescriptor() SourceDescriptor {
	return SourceDescriptor{
		Provider:       "github",
		DisplayName:    "GitHub",
		Capabilities:   Capabilities{Backfill: true, AuthModes: []AuthMode{AuthToken}},
		DefaultPublish: PublishShared,
		ConfigFields: []ConfigField{
			{Key: "repos", Label: "Repositories", Required: true},
			{Key: "token", Label: "Token", Required: true, Secret: true},
		},
	}
}

func TestConnectionValidateAcceptsValid(t *testing.T) {
	// Arrange
	conn := validConnection()

	// Act
	err := conn.Validate()

	// Assert
	if err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestConnectionValidateRejectsInvalid(t *testing.T) {
	cases := map[string]func(*Connection){
		"empty id":          func(c *Connection) { c.ID = "" },
		"uppercase id":      func(c *Connection) { c.ID = "GitHub" },
		"underscore id":     func(c *Connection) { c.ID = "git_hub" },
		"slash id":          func(c *Connection) { c.ID = "a/b" },
		"dot id":            func(c *Connection) { c.ID = ".." },
		"leading dash":      func(c *Connection) { c.ID = "-a" },
		"trailing dash":     func(c *Connection) { c.ID = "a-" },
		"long id":           func(c *Connection) { c.ID = strings.Repeat("a", 65) },
		"bad provider":      func(c *Connection) { c.Provider = "Git Hub" },
		"control account":   func(c *Connection) { c.Account = "a\nb" },
		"relative base":     func(c *Connection) { c.BaseURL = "api.github.com" },
		"userinfo base":     func(c *Connection) { c.BaseURL = "https://user:pw@host" },
		"fragment base":     func(c *Connection) { c.BaseURL = "https://host/#x" },
		"query base":        func(c *Connection) { c.BaseURL = "https://host/?access_token=x" },
		"duplicate project": func(c *Connection) { c.Projects = []string{"a", "a"} },
		"blank project":     func(c *Connection) { c.Projects = []string{" "} },
		"bad schedule":      func(c *Connection) { c.Schedule = "*/5 * * * *" },
		"short schedule":    func(c *Connection) { c.Schedule = "@every 10s" },
		"bad publish":       func(c *Connection) { c.Publish = "public" },
		"bad setting key":   func(c *Connection) { c.Settings = map[string]string{"api-token": "x"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			conn := validConnection()
			mutate(&conn)

			// Act
			err := conn.Validate()

			// Assert
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("Validate() = %v, want validation error", err)
			}
		})
	}
}

func TestScheduleAcceptsKnownForms(t *testing.T) {
	for _, schedule := range []string{"", "manual", "@hourly", "@daily", "@every 1m", "@every 2h30m"} {
		if err := ValidateSchedule(schedule); err != nil {
			t.Errorf("ValidateSchedule(%q) = %v", schedule, err)
		}
	}
}

func TestDescriptorValidateConnection(t *testing.T) {
	descriptor := testDescriptor()
	cases := map[string]struct {
		mutate func(*Connection)
		ok     bool
	}{
		"valid":             {mutate: func(*Connection) {}, ok: true},
		"other provider":    {mutate: func(c *Connection) { c.Provider = "gitlab" }},
		"missing required":  {mutate: func(c *Connection) { c.Settings = nil }},
		"unknown setting":   {mutate: func(c *Connection) { c.Settings["extra"] = "x" }},
		"secret in setting": {mutate: func(c *Connection) { c.Settings["token"] = "ghp_x" }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			conn := validConnection()
			tc.mutate(&conn)

			// Act
			err := descriptor.ValidateConnection(conn)

			// Assert
			if tc.ok != (err == nil) {
				t.Fatalf("ValidateConnection() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestDescriptorValidateRejectsDuplicatesAndBadValues(t *testing.T) {
	cases := map[string]func(*SourceDescriptor){
		"duplicate field": func(d *SourceDescriptor) {
			d.ConfigFields = append(d.ConfigFields, ConfigField{Key: "repos", Label: "x"})
		},
		"no auth modes":   func(d *SourceDescriptor) { d.Capabilities.AuthModes = nil },
		"bad auth mode":   func(d *SourceDescriptor) { d.Capabilities.AuthModes = []AuthMode{"magic"} },
		"empty publish":   func(d *SourceDescriptor) { d.DefaultPublish = "" },
		"bad field key":   func(d *SourceDescriptor) { d.ConfigFields[0].Key = "Repos" },
		"no display name": func(d *SourceDescriptor) { d.DisplayName = " " },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			descriptor := testDescriptor()
			mutate(&descriptor)
			if err := descriptor.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

func TestEffectivePublishFallsBackToDescriptor(t *testing.T) {
	// Arrange
	conn := validConnection()
	descriptor := testDescriptor()

	// Act
	defaulted := conn.EffectivePublish(descriptor)
	conn.Publish = PublishLocalOnly
	overridden := conn.EffectivePublish(descriptor)

	// Assert
	if defaulted != PublishShared || overridden != PublishLocalOnly {
		t.Fatalf("EffectivePublish = %q, %q", defaulted, overridden)
	}
}

func TestIdentityCanonicalizesEndpointAndAccount(t *testing.T) {
	// Arrange
	a := Connection{ID: "a", Provider: "gitlab", BaseURL: "HTTPS://GitLab.example.com:443/", Account: "Bot"}
	b := Connection{ID: "b", Provider: "gitlab", BaseURL: "https://gitlab.example.com", Account: "bot"}
	c := Connection{ID: "c", Provider: "gitlab", BaseURL: "https://gitlab.example.com:8443", Account: "bot"}

	// Act / Assert
	if a.Identity() != b.Identity() {
		t.Fatalf("identities differ: %q vs %q", a.Identity(), b.Identity())
	}
	if a.Identity() == c.Identity() {
		t.Fatalf("different ports share identity %q", c.Identity())
	}
}

func TestCredentialKey(t *testing.T) {
	// Act
	key, err := CredentialKey("gitlab-work", "token")
	_, badField := CredentialKey("gitlab-work", "api_token")
	_, badID := CredentialKey("../x", "token")

	// Assert
	if err != nil || key != "conn/gitlab-work/token" {
		t.Fatalf("CredentialKey = %q, %v", key, err)
	}
	if !errors.Is(badField, ErrValidation) || !errors.Is(badID, ErrValidation) {
		t.Fatalf("invalid inputs accepted: %v, %v", badField, badID)
	}
}

func TestItemRefValidate(t *testing.T) {
	cases := map[string]ItemRef{
		"empty external":   {ConnectionID: "c", ExternalID: ""},
		"nul external":     {ConnectionID: "c", ExternalID: "a\x00b"},
		"control external": {ConnectionID: "c", ExternalID: "a\nb"},
		"bad utf8":         {ConnectionID: "c", ExternalID: "\xff"},
		"long external":    {ConnectionID: "c", ExternalID: strings.Repeat("x", 1025)},
		"bad connection":   {ConnectionID: "C", ExternalID: "1"},
	}
	for name, ref := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ref.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

func TestItemKeyIsStableAndPathSafe(t *testing.T) {
	// Arrange
	ref := ItemRef{ConnectionID: "github-acme", ExternalID: "acme/widgets#12"}

	// Act
	first, second := ref.Key(), ref.Key()

	// Assert
	if first != second {
		t.Fatalf("Key not stable: %q vs %q", first, second)
	}
	prefix, hash, ok := strings.Cut(first, "/")
	if !ok || prefix != "github-acme" || len(hash) != 40 || strings.Trim(hash, "0123456789abcdef") != "" {
		t.Fatalf("Key = %q, want github-acme/<40 hex>", first)
	}
}

func TestItemKeyUniqueAcrossProvidersHostsAccountsAndLookalikeIDs(t *testing.T) {
	// Arrange: the same repository path on two providers, two hosts and two
	// accounts (each a separate connection), plus external IDs that would
	// collide after file-name sanitization.
	refs := []ItemRef{
		{ConnectionID: "github-com-alice", ExternalID: "acme/widgets#1"},
		{ConnectionID: "github-ghe-alice", ExternalID: "acme/widgets#1"},
		{ConnectionID: "github-com-bob", ExternalID: "acme/widgets#1"},
		{ConnectionID: "gitlab-com-alice", ExternalID: "acme/widgets#1"},
		{ConnectionID: "github-com-alice", ExternalID: "acme_widgets#1"},
		{ConnectionID: "github-com-alice", ExternalID: "ACME/widgets#1"},
		{ConnectionID: "github-com-alice", ExternalID: "acme/widgets#1 "},
		{ConnectionID: "a", ExternalID: "b-c"},
		{ConnectionID: "a-b", ExternalID: "c"},
	}

	// Act
	seen := map[string]ItemRef{}
	for _, ref := range refs {
		key := ref.Key()

		// Assert
		if previous, dup := seen[key]; dup {
			t.Fatalf("Key collision %q for %+v and %+v", key, previous, ref)
		}
		seen[key] = ref
	}
}

func validItem() Item {
	return Item{
		Ref:       ItemRef{ConnectionID: "github-acme", ExternalID: "acme/widgets#12"},
		Kind:      ItemIssue,
		Title:     "Fix   login",
		Body:      "line one\nline two\n",
		URL:       "https://github.com/acme/widgets/issues/12",
		Author:    "alice",
		Labels:    []string{"bug", "auth"},
		CreatedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC),
	}
}

func TestItemHashIgnoresWhitespaceLineEndingsOrderAndObservedAt(t *testing.T) {
	// Arrange
	base := validItem()
	variant := validItem()
	variant.Title = " Fix login "
	variant.Body = "\r\nline one  \r\nline two\t\r\n\r\n"
	variant.Labels = []string{"auth", " bug "}
	variant.ObservedAt = time.Now()
	variant.CreatedAt = base.CreatedAt.In(time.FixedZone("x", 3*3600))

	// Act
	baseHash, variantHash := base.ComputeHash(), variant.ComputeHash()

	// Assert
	if baseHash != variantHash {
		t.Fatalf("hash changed with formatting: %s vs %s", baseHash, variantHash)
	}
	if !strings.HasPrefix(baseHash, ItemHashPrefix) {
		t.Fatalf("hash %q lacks prefix", baseHash)
	}
}

func TestItemHashChangesWithContent(t *testing.T) {
	base := validItem().ComputeHash()
	mutations := map[string]func(*Item){
		"title":    func(i *Item) { i.Title = "Fix logout" },
		"body":     func(i *Item) { i.Body = "line one\n\nline two" },
		"label":    func(i *Item) { i.Labels = []string{"bug"} },
		"updated":  func(i *Item) { i.UpdatedAt = i.UpdatedAt.Add(time.Second) },
		"kind":     func(i *Item) { i.Kind = ItemPullRequest },
		"acl":      func(i *Item) { i.ACL = []string{"group:eng"} },
		"deleted":  func(i *Item) { i.Deleted = true },
		"project":  func(i *Item) { i.ProjectID = "lv24" },
		"state":    func(i *Item) { i.State = ItemStateMerged },
		"boundary": func(i *Item) { i.Title, i.Body = "Fix   loginline one", "\nline two" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			item := validItem()
			mutate(&item)
			if item.ComputeHash() == base {
				t.Fatalf("hash unchanged after %s change", name)
			}
		})
	}
}

func TestItemValidate(t *testing.T) {
	// Arrange
	good := validItem().WithHash()
	stale := good
	stale.Title = "changed"
	tombstone := Item{Ref: good.Ref, Deleted: true}
	untitled := validItem()
	untitled.Title = " "
	badKind := validItem()
	badKind.Kind = "ticket"
	badState := validItem()
	badState.State = "reopened"

	// Act / Assert
	if err := good.Validate(); err != nil {
		t.Fatalf("valid item: %v", err)
	}
	if err := tombstone.Validate(); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	for name, item := range map[string]Item{"stale hash": stale, "untitled": untitled, "bad kind": badKind, "bad state": badState} {
		if err := item.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: Validate() = %v", name, err)
		}
	}
}

func TestItemHashIsUnchangedWithoutState(t *testing.T) {
	// The literal is the hash of validItem before Item.State existed. Empty
	// state must append nothing so stored items do not re-upsert on upgrade.
	const legacyHash = "sha256:8596fbc6cf8fc5fe7fc25e0e0ef8a16f1bda4d3ae70cc1266c36ec04067c8f46"
	item := validItem()
	if got := item.ComputeHash(); got != legacyHash {
		t.Fatalf("legacy item hash = %s, want %s", got, legacyHash)
	}
	item.State = ItemStateClosed
	if item.ComputeHash() == legacyHash {
		t.Fatal("state did not change the hash")
	}
	item.State = ""
	if got := item.ComputeHash(); got != legacyHash {
		t.Fatalf("clearing state did not restore the hash: %s", got)
	}
}

func TestCheckpointIsOpaqueAndCloned(t *testing.T) {
	// Arrange: arbitrary bytes, including NUL, invalid UTF-8 and JSON-like text.
	data := []byte{0x00, 0xff, '{', '"', 'u', 'i', 'd', '"', '}', 0x80}
	checkpoint := Checkpoint{Stream: "INBOX", Version: 3, Data: data}

	// Act
	clone := checkpoint.Clone()
	data[0] = 0x42
	encoded, err := json.Marshal(clone)
	var decoded Checkpoint
	if err == nil {
		err = json.Unmarshal(encoded, &decoded)
	}

	// Assert
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if clone.Data[0] != 0x00 {
		t.Fatalf("Clone shares Data")
	}
	if !decoded.Equal(clone) {
		t.Fatalf("round trip changed checkpoint: %+v vs %+v", decoded, clone)
	}
	if err := clone.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestCheckpointValidate(t *testing.T) {
	cases := map[string]Checkpoint{
		"no stream":   {Version: 1},
		"bad stream":  {Stream: "a b", Version: 1},
		"dot stream":  {Stream: ".hidden", Version: 1},
		"no version":  {Stream: "issues"},
		"too large":   {Stream: "issues", Version: 1, Data: make([]byte, MaxCheckpointBytes+1)},
		"long stream": {Stream: strings.Repeat("s", 129), Version: 1},
	}
	for name, checkpoint := range cases {
		t.Run(name, func(t *testing.T) {
			if err := checkpoint.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

func TestRunStatsAddAndValidate(t *testing.T) {
	// Arrange
	first := RunStats{Seen: 3, Upserted: 2, Unchanged: 1, Batches: 1}
	second := RunStats{Seen: 2, Deleted: 1, Skipped: 1, Batches: 1, Truncated: TruncatedLimit}

	// Act
	total := first.Add(second)

	// Assert
	if total.Seen != 5 || total.Batches != 2 || total.Truncated != TruncatedLimit {
		t.Fatalf("Add = %+v", total)
	}
	if err := total.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if err := (RunStats{Seen: 1}).Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("inconsistent stats accepted: %v", err)
	}
}

func TestConnectorRunJSONUnchangedForLegacyRuns(t *testing.T) {
	// Arrange
	run := ConnectorRun{ID: "r1", Source: "github", StartedAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), Status: ConnectorSuccess}

	// Act
	encoded, err := json.Marshal(run)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"r1","source":"github","startedAt":"2026-11-09T00:00:00Z","status":"success"}`
	if string(encoded) != want {
		t.Fatalf("JSON = %s, want %s", encoded, want)
	}
}

func TestConnectorRunValidatesItemFields(t *testing.T) {
	// Arrange
	run := ConnectorRun{Source: "github", Status: ConnectorPartial, ConnectionID: "github-acme", Stats: &RunStats{Seen: 1, Upserted: 1}}
	bad := run
	bad.ConnectionID = "Bad/ID"

	// Act / Assert
	if err := run.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if err := bad.Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad connection id accepted: %v", err)
	}
}
