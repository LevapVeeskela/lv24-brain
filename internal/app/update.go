package app

import (
	"context"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

// Update checks the release channel and optionally verifies, downloads and
// applies a newer engine binary. Check-only runs and runs without Yes never
// change the installed binary.
func (a *App) Update(ctx context.Context, opts UpdateOptions) (UpdateResult, error) {
	if a.deps.Updater == nil {
		return UpdateResult{}, notImplemented("update")
	}
	channel := strings.ToLower(strings.TrimSpace(opts.Channel))
	if channel == "" {
		channel = strings.ToLower(strings.TrimSpace(a.cfg.UpdateChannel))
	}
	if channel == "" {
		channel = config.ChannelStable
	}
	current := VersionInfo{Version: version.Version, Channel: a.cfg.UpdateChannel}
	if current.Channel == "" {
		current.Channel = config.ChannelStable
	}
	release, err := a.deps.Updater.Check(ctx, channel)
	if err != nil {
		return UpdateResult{Current: current}, err
	}
	latest := VersionInfo{Version: release.Version, Channel: release.Channel}
	if latest.Channel == "" {
		latest.Channel = channel
	}
	result := UpdateResult{Current: current, Latest: latest}
	if opts.Check || !opts.Yes {
		return result, nil
	}
	if compareVersions(latest.Version, current.Version) <= 0 {
		return result, nil
	}
	artifact, err := a.deps.Updater.Download(ctx, release)
	if err != nil {
		return result, err
	}
	if err := a.deps.Updater.Apply(ctx, artifact); err != nil {
		return result, err
	}
	result.Applied = true
	return result, nil
}
