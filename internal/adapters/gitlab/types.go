package gitlab

import "time"

type project struct {
	ID                int       `json:"id"`
	PathWithNamespace string    `json:"path_with_namespace"`
	Name              string    `json:"name"`
	Description       string    `json:"description"`
	WebURL            string    `json:"web_url"`
	DefaultBranch     string    `json:"default_branch"`
	Visibility        string    `json:"visibility"`
	Archived          bool      `json:"archived"`
	StarCount         int       `json:"star_count"`
	ForksCount        int       `json:"forks_count"`
	OpenIssuesCount   int       `json:"open_issues_count"`
	LastActivityAt    time.Time `json:"last_activity_at"`
}

type author struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

type mergeRequest struct {
	IID            int        `json:"iid"`
	Title          string     `json:"title"`
	State          string     `json:"state"`
	WebURL         string     `json:"web_url"`
	Author         author     `json:"author"`
	Draft          bool       `json:"draft"`
	WorkInProgress bool       `json:"work_in_progress"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	MergedAt       *time.Time `json:"merged_at"`
	ClosedAt       *time.Time `json:"closed_at"`
	SourceBranch   string     `json:"source_branch"`
	TargetBranch   string     `json:"target_branch"`
	Labels         []string   `json:"labels"`
}

type issue struct {
	IID            int        `json:"iid"`
	Title          string     `json:"title"`
	State          string     `json:"state"`
	WebURL         string     `json:"web_url"`
	Author         author     `json:"author"`
	UserNotesCount int        `json:"user_notes_count"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ClosedAt       *time.Time `json:"closed_at"`
	Labels         []string   `json:"labels"`
}

type commit struct {
	ID           string    `json:"id"`
	ShortID      string    `json:"short_id"`
	Title        string    `json:"title"`
	Message      string    `json:"message"`
	AuthorName   string    `json:"author_name"`
	AuthorEmail  string    `json:"author_email"`
	AuthoredDate time.Time `json:"authored_date"`
	WebURL       string    `json:"web_url"`
}
