package agentcontrol

import (
	"context"
	"errors"

	"github.com/levapveeskela/lv24-brain/internal/adapters/localapi"
	"github.com/levapveeskela/lv24-brain/internal/app"
)

var errSyncUnavailable = errors.New("sync is not available in this mode")

// syncService adapts the curated app's git sync use case to the local API.
type syncService struct {
	app SyncApp
}

func newSyncService(application SyncApp) localapi.SyncService {
	return syncService{app: application}
}

func (s syncService) Status(ctx context.Context) (localapi.SyncView, error) {
	if s.app == nil {
		return localapi.SyncView{}, errSyncUnavailable
	}
	result, err := s.app.Sync(ctx, app.SyncOptions{StatusOnly: true})
	return syncView(result), err
}

func (s syncService) Sync(ctx context.Context, push bool) (localapi.SyncView, error) {
	if s.app == nil {
		return localapi.SyncView{}, errSyncUnavailable
	}
	result, err := s.app.Sync(ctx, app.SyncOptions{Push: push})
	return syncView(result), err
}

func syncView(result app.SyncResult) localapi.SyncView {
	view := localapi.SyncView{
		Provider:        result.Provider,
		Branch:          result.Branch,
		Ahead:           result.Ahead,
		Behind:          result.Behind,
		Changed:         result.Changed,
		Dirty:           result.Dirty,
		Unmerged:        len(result.Conflicts) > 0,
		Conflicts:       append([]string{}, result.Conflicts...),
		ConflictRecords: append([]string{}, result.ConflictRecords...),
		Recovered:       result.Recovered,
		Reindexed:       result.Reindexed,
	}
	if view.Conflicts == nil {
		view.Conflicts = []string{}
	}
	if view.ConflictRecords == nil {
		view.ConflictRecords = []string{}
	}
	if result.LastSynced.Value != "" {
		view.LastSynced = &localapi.SyncRevisionView{
			Kind:  string(result.LastSynced.Kind),
			Value: result.LastSynced.Value,
			At:    result.LastSynced.At,
		}
	}
	return view
}
