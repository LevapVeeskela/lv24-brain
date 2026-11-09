package github

import "time"

type repository struct {
	FullName        string    `json:"full_name"`
	Description     string    `json:"description"`
	HTMLURL         string    `json:"html_url"`
	DefaultBranch   string    `json:"default_branch"`
	Private         bool      `json:"private"`
	Archived        bool      `json:"archived"`
	Fork            bool      `json:"fork"`
	StargazersCount int       `json:"stargazers_count"`
	ForksCount      int       `json:"forks_count"`
	OpenIssuesCount int       `json:"open_issues_count"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type label struct {
	Name string `json:"name"`
}

type user struct {
	Login string `json:"login"`
}

type pullRequest struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	HTMLURL   string     `json:"html_url"`
	User      user       `json:"user"`
	Draft     bool       `json:"draft"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	MergedAt  *time.Time `json:"merged_at"`
	Labels    []label    `json:"labels"`
	Head      struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type issue struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	HTMLURL     string     `json:"html_url"`
	User        user       `json:"user"`
	Comments    int        `json:"comments"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	Labels      []label    `json:"labels"`
	PullRequest *struct{}  `json:"pull_request"`
}

type commit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Author struct {
			Name  string    `json:"name"`
			Email string    `json:"email"`
			Date  time.Time `json:"date"`
		} `json:"author"`
		Message string `json:"message"`
	} `json:"commit"`
	Author *user `json:"author"`
}
