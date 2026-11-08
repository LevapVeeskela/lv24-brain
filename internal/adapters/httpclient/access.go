package httpclient

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// ProjectAccess is a project's restriction flag and members (admin only).
type ProjectAccess struct {
	ProjectID  string                 `json:"projectId"`
	Restricted bool                   `json:"restricted"`
	Members    []domain.ProjectMember `json:"items"`
}

// ListProjectMembers loads a project's access configuration.
func (c *Client) ListProjectMembers(ctx context.Context, projectID string) (ProjectAccess, error) {
	if err := requireValue("project.id", projectID); err != nil {
		return ProjectAccess{}, err
	}
	var access ProjectAccess
	err := c.do(ctx, http.MethodGet, "/v1/projects/"+url.PathEscape(projectID)+"/members", nil, nil, &access)
	return access, err
}

// PutProjectMember adds or updates a membership; user is a login or user id.
func (c *Client) PutProjectMember(ctx context.Context, projectID, user string, role domain.ProjectRole) (domain.ProjectMember, error) {
	if err := requireValue("project.id", projectID); err != nil {
		return domain.ProjectMember{}, err
	}
	if err := requireValue("member.user", user); err != nil {
		return domain.ProjectMember{}, err
	}
	var member domain.ProjectMember
	path := "/v1/projects/" + url.PathEscape(projectID) + "/members/" + url.PathEscape(user)
	err := c.do(ctx, http.MethodPut, path, nil, map[string]string{"role": string(role)}, &member)
	return member, err
}

// RemoveProjectMember removes a membership; the project stays restricted.
func (c *Client) RemoveProjectMember(ctx context.Context, projectID, user string) error {
	if err := requireValue("project.id", projectID); err != nil {
		return err
	}
	if err := requireValue("member.user", user); err != nil {
		return err
	}
	path := "/v1/projects/" + url.PathEscape(projectID) + "/members/" + url.PathEscape(user)
	return c.do(ctx, http.MethodDelete, path, nil, nil, nil)
}

// SetProjectRestricted restricts or reopens a project.
func (c *Client) SetProjectRestricted(ctx context.Context, projectID string, restricted bool) (ProjectAccess, error) {
	if err := requireValue("project.id", projectID); err != nil {
		return ProjectAccess{}, err
	}
	var access ProjectAccess
	err := c.do(ctx, http.MethodPut, "/v1/projects/"+url.PathEscape(projectID)+"/access", nil,
		map[string]bool{"restricted": restricted}, &access)
	return access, err
}

// APITokenRequest describes a token to mint. Empty Scopes and ProjectIDs mean
// unscoped / every project.
type APITokenRequest struct {
	Name          string   `json:"name"`
	Role          string   `json:"role,omitempty"`
	Scopes        []string `json:"scopes,omitempty"`
	ProjectIDs    []string `json:"projectIds,omitempty"`
	ExpiresInDays *int     `json:"expiresInDays,omitempty"`
}

// APIToken is a minted token's public record.
type APIToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"userId"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes,omitempty"`
	ProjectIDs []string   `json:"projectIds,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
}

// CreateAPIToken mints a token for the signed-in user and returns the raw
// value (shown once) with its record.
func (c *Client) CreateAPIToken(ctx context.Context, req APITokenRequest) (string, APIToken, error) {
	if err := requireValue("token.name", req.Name); err != nil {
		return "", APIToken{}, err
	}
	var out struct {
		Token    string   `json:"token"`
		APIToken APIToken `json:"apiToken"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/tokens", nil, req, &out); err != nil {
		return "", APIToken{}, err
	}
	return out.Token, out.APIToken, nil
}
