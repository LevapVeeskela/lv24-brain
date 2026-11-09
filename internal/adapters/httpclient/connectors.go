package httpclient

import (
	"context"
	"net/http"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type connectorRunRequest struct {
	Name      string     `json:"name"`
	ProjectID string     `json:"projectId,omitempty"`
	Projects  []string   `json:"projects,omitempty"`
	Since     *time.Time `json:"since,omitempty"`
	Limit     int        `json:"limit,omitempty"`
}

// RunConnector asks the server to run one read-only connector. The returned
// run is a completed record even when its status is failed/partial; a server
// error (validation, permissions, persistence) is returned as an error.
func (c *Client) RunConnector(ctx context.Context, name string, projects []string, since *time.Time, limit int) (domain.ConnectorRun, error) {
	return c.RunConnectorMapped(ctx, name, projects, since, limit, "")
}

// RunConnectorMapped maps raw items to a brain project on the server while
// retaining the repository/project target filters of RunConnector.
func (c *Client) RunConnectorMapped(ctx context.Context, name string, projects []string, since *time.Time, limit int, projectID string) (domain.ConnectorRun, error) {
	var run domain.ConnectorRun
	if err := c.executeWithClient(ctx, http.MethodPost, "/v1/connectors/run", nil, connectorRunRequest{
		Name:      name,
		ProjectID: projectID,
		Projects:  projects,
		Since:     since,
		Limit:     limit,
	}, &run, "", false, true, c.connectorHTTPClient); err != nil {
		return domain.ConnectorRun{}, err
	}
	return run, nil
}
