package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

const (
	doctorOK   = "ok"
	doctorWarn = "warn"
	doctorFail = "fail"
)

// Doctor checks the configuration, data directory, schema, index and git
// availability for the selected mode. It reports problems instead of failing,
// so a broken instance can still be diagnosed.
func (a *App) Doctor(ctx context.Context, opts DoctorOptions) (DoctorReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return DoctorReport{}, err
	}
	checks := make([]DoctorCheck, 0, 6)
	checks = append(checks, a.doctorConfig(opts.ConfigPath))
	if a.cfg.Mode == config.ModeServer {
		checks = append(checks, a.doctorServer(ctx))
	}
	checks = append(checks, a.doctorDataDir())
	checks = append(checks, a.doctorSchema())
	checks = append(checks, a.doctorIndex(ctx))
	checks = append(checks, a.doctorGit(ctx))
	return DoctorReport{Checks: checks}, nil
}

func (a *App) doctorConfig(configPath string) DoctorCheck {
	message := "configuration is valid"
	if configPath != "" {
		info, err := os.Stat(configPath)
		if err != nil {
			if os.IsNotExist(err) {
				return DoctorCheck{Name: "config", Status: doctorFail, Message: "not found: " + configPath}
			}
			return DoctorCheck{Name: "config", Status: doctorFail, Message: err.Error()}
		}
		if info.IsDir() {
			return DoctorCheck{Name: "config", Status: doctorFail, Message: "is a directory: " + configPath}
		}
		message = "configuration is valid: " + configPath
	}
	if err := a.cfg.Validate(); err != nil {
		return DoctorCheck{Name: "config", Status: doctorFail, Message: err.Error()}
	}
	return DoctorCheck{Name: "config", Status: doctorOK, Message: message}
}

func (a *App) doctorDataDir() DoctorCheck {
	switch a.cfg.Mode {
	case config.ModeServer:
		return DoctorCheck{Name: "data_dir", Status: doctorOK, Message: "not used in server mode"}
	case config.ModeLocal, config.ModeGit:
	default:
		return DoctorCheck{Name: "data_dir", Status: doctorFail, Message: "unknown mode " + a.cfg.Mode}
	}
	dataDir := strings.TrimSpace(a.cfg.DataDir)
	if dataDir == "" {
		return DoctorCheck{Name: "data_dir", Status: doctorFail, Message: "config.data_dir is empty"}
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return DoctorCheck{Name: "data_dir", Status: doctorFail, Message: "does not exist: " + dataDir}
		}
		return DoctorCheck{Name: "data_dir", Status: doctorFail, Message: err.Error()}
	}
	if !info.IsDir() {
		return DoctorCheck{Name: "data_dir", Status: doctorFail, Message: "is not a directory: " + dataDir}
	}
	if projectsInfo, projectsErr := os.Stat(filepath.Join(dataDir, "projects")); projectsErr != nil || !projectsInfo.IsDir() {
		return DoctorCheck{Name: "data_dir", Status: doctorWarn, Message: "projects/ is missing; run brain init"}
	}
	return DoctorCheck{Name: "data_dir", Status: doctorOK, Message: "exists: " + dataDir}
}

func (a *App) doctorSchema() DoctorCheck {
	if minimum := strings.TrimSpace(a.cfg.MinEngineVersion); minimum != "" && compareVersions(version.Version, minimum) < 0 {
		return DoctorCheck{
			Name:    "schema",
			Status:  doctorFail,
			Message: fmt.Sprintf("engine %s is older than the data requirement %s", version.Version, minimum),
		}
	}
	switch {
	case a.cfg.SchemaVersion > config.CurrentSchemaVersion:
		return DoctorCheck{
			Name:   "schema",
			Status: doctorFail,
			Message: fmt.Sprintf("data schema %d requires a newer engine (this engine supports %d)",
				a.cfg.SchemaVersion, config.CurrentSchemaVersion),
		}
	case a.cfg.SchemaVersion < config.CurrentSchemaVersion:
		return DoctorCheck{
			Name:   "schema",
			Status: doctorWarn,
			Message: fmt.Sprintf("data schema %d is older than engine schema %d",
				a.cfg.SchemaVersion, config.CurrentSchemaVersion),
		}
	default:
		return DoctorCheck{Name: "schema", Status: doctorOK, Message: fmt.Sprintf("schema %d", a.cfg.SchemaVersion)}
	}
}

func (a *App) doctorIndex(ctx context.Context) DoctorCheck {
	if a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: "index", Status: doctorOK, Message: "server mode uses the remote database"}
	}
	if a.deps.Index == nil {
		return DoctorCheck{Name: "index", Status: doctorFail, Message: "local search index is not available"}
	}
	if _, err := a.deps.Index.Search(ctx, ports.Query{Text: "brain-doctor", Limit: 1}); err != nil {
		return DoctorCheck{Name: "index", Status: doctorFail, Message: err.Error()}
	}
	return DoctorCheck{Name: "index", Status: doctorOK, Message: "SQLite FTS5 is usable"}
}

func (a *App) doctorServer(ctx context.Context) DoctorCheck {
	if strings.TrimSpace(a.cfg.ServerURL) == "" {
		return DoctorCheck{Name: "server", Status: doctorFail, Message: "config.server_url is empty"}
	}
	if a.deps.Store == nil {
		return DoctorCheck{
			Name:    "server",
			Status:  doctorFail,
			Message: "server client is not configured; set BRAIN_TOKEN or run brain secret set server-token",
		}
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	revision, err := a.deps.Store.CurrentRevision(checkCtx)
	if err != nil {
		return DoctorCheck{Name: "server", Status: doctorFail, Message: "server API: " + err.Error()}
	}
	return DoctorCheck{Name: "server", Status: doctorOK, Message: "API reachable (revision " + revision.Value + ")"}
}

func (a *App) doctorGit(ctx context.Context) DoctorCheck {
	switch a.cfg.Mode {
	case config.ModeLocal:
		return DoctorCheck{Name: "git", Status: doctorOK, Message: "not required in local mode"}
	case config.ModeServer:
		return DoctorCheck{Name: "git", Status: doctorOK, Message: "not used in server mode"}
	case config.ModeGit:
	default:
		return DoctorCheck{Name: "git", Status: doctorFail, Message: "unknown mode " + a.cfg.Mode}
	}
	if a.deps.Sync == nil {
		return DoctorCheck{Name: "git", Status: doctorFail, Message: "git sync provider is not configured"}
	}
	status, err := a.deps.Sync.Status(ctx)
	if err != nil {
		return DoctorCheck{Name: "git", Status: doctorFail, Message: err.Error()}
	}
	message := "git is available"
	if status.Branch != "" {
		message = fmt.Sprintf("branch %s, ahead %d, behind %d", status.Branch, status.Ahead, status.Behind)
	}
	return DoctorCheck{Name: "git", Status: doctorOK, Message: message}
}
