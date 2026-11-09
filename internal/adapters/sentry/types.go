package sentry

import "time"

type sentryOrg struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type sentryProjectRef struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type sentryIssue struct {
	ID        string           `json:"id"`
	ShortID   string           `json:"shortId"`
	Title     string           `json:"title"`
	Culprit   string           `json:"culprit"`
	Permalink string           `json:"permalink"`
	Level     string           `json:"level"`
	Logger    string           `json:"logger"`
	FirstSeen time.Time        `json:"firstSeen"`
	LastSeen  time.Time        `json:"lastSeen"`
	Project   sentryProjectRef `json:"project"`
	Metadata  map[string]any   `json:"metadata"`
}

type sentryEvent struct {
	ID          string        `json:"id"`
	Message     string        `json:"message"`
	Title       string        `json:"title"`
	Culprit     string        `json:"culprit"`
	DateCreated time.Time     `json:"dateCreated"`
	Entries     []sentryEntry `json:"entries"`
}

type sentryEntry struct {
	Type string          `json:"type"`
	Data sentryEntryData `json:"data"`
}

type sentryEntryData struct {
	Values []sentryException `json:"values"`
}

type sentryException struct {
	Type       string            `json:"type"`
	Value      string            `json:"value"`
	Stacktrace *sentryStacktrace `json:"stacktrace"`
}

type sentryStacktrace struct {
	Frames []sentryFrame `json:"frames"`
}

type sentryFrame struct {
	Filename string `json:"filename"`
	Function string `json:"function"`
	Module   string `json:"module"`
	Lineno   int    `json:"lineno"`
}
