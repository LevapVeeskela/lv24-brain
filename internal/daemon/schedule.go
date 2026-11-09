package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const scheduleStateVersion = 1

// ScheduleState is persisted in agent/schedule.json. NextRun is UTC RFC3339Nano.
type ScheduleState struct {
	Version int                      `json:"version"`
	Entries map[string]ScheduleEntry `json:"entries"`
}

type ScheduleEntry struct {
	Schedule string    `json:"schedule"`
	NextRun  time.Time `json:"next_run"`
}

// ParseSchedule returns the base interval, or zero for manual schedules.
func ParseSchedule(schedule string) (time.Duration, error) {
	switch schedule {
	case "", "manual":
		return 0, nil
	case "@hourly":
		return time.Hour, nil
	case "@daily":
		return 24 * time.Hour, nil
	}
	if !strings.HasPrefix(schedule, "@every ") {
		return 0, fmt.Errorf("invalid schedule %q", schedule)
	}
	interval, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(schedule, "@every ")))
	if err != nil {
		return 0, fmt.Errorf("invalid schedule duration: %w", err)
	}
	if interval < time.Minute {
		return 0, errors.New("schedule interval must be at least 1m")
	}
	return interval, nil
}

// DeterministicJitter is stable per connection and scheduled timestamp, from
// zero through (but excluding) maxJitter. The scheduler uses 5% of interval.
func DeterministicJitter(connectionID string, scheduled time.Time, maxJitter time.Duration) time.Duration {
	if maxJitter <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(connectionID + "\x00" + scheduled.UTC().Format(time.RFC3339Nano)))
	return time.Duration(binary.BigEndian.Uint64(sum[:8]) % uint64(maxJitter))
}

func nextRun(connectionID, schedule string, from time.Time) (time.Time, error) {
	interval, err := ParseSchedule(schedule)
	if err != nil {
		return time.Time{}, err
	}
	if interval == 0 {
		return time.Time{}, nil
	}
	base := from.UTC().Add(interval)
	return base.Add(DeterministicJitter(connectionID, base, interval/20)), nil
}

func loadScheduleState(path string) (ScheduleState, error) {
	state := ScheduleState{Version: scheduleStateVersion, Entries: make(map[string]ScheduleEntry)}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, fmt.Errorf("daemon: decode schedule state: %w", err)
	}
	if state.Version != scheduleStateVersion || state.Entries == nil {
		return ScheduleState{}, errors.New("daemon: unsupported or invalid schedule state")
	}
	for id, entry := range state.Entries {
		if strings.TrimSpace(entry.Schedule) == "" || entry.NextRun.IsZero() {
			delete(state.Entries, id)
			continue
		}
		if _, err := ParseSchedule(entry.Schedule); err != nil {
			delete(state.Entries, id)
		}
	}
	return state, nil
}

func saveScheduleState(path string, state ScheduleState) error {
	if state.Entries == nil {
		state.Entries = make(map[string]ScheduleEntry)
	}
	state.Version = scheduleStateVersion
	return writeJSONAtomic(path, state, 0o600)
}

func (a *Agent) scheduler(ctx context.Context) {
	state, err := loadScheduleState(a.statePath)
	if err != nil {
		a.logger.Printf("schedule state load failed: %v", err)
		state = ScheduleState{Version: scheduleStateVersion, Entries: make(map[string]ScheduleEntry)}
	}
	if err := a.refreshSchedule(ctx, &state); err != nil {
		a.logger.Printf("schedule refresh failed: %v", err)
	}
	for {
		if ctx.Err() != nil {
			return
		}
		now := a.clock.Now().UTC()
		ids := make([]string, 0, len(state.Entries))
		for id := range state.Entries {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			entry := state.Entries[id]
			if entry.NextRun.IsZero() || now.Before(entry.NextRun) {
				continue
			}
			if _, err := a.QueueConnection(ctx, id); err != nil && !errors.Is(err, ErrJobActive) {
				a.logger.Printf("schedule queue failed connection=%s error=%v", id, safeError(err))
			}
			// Catch up once after sleep; never replay a backlog of missed intervals.
			entry.NextRun, err = nextRun(id, entry.Schedule, now)
			if err != nil {
				delete(state.Entries, id)
				continue
			}
			state.Entries[id] = entry
		}
		if err := saveScheduleState(a.statePath, state); err != nil {
			a.logger.Printf("schedule state save failed: %v", err)
		}
		delay := time.Minute
		for _, entry := range state.Entries {
			if entry.NextRun.IsZero() {
				continue
			}
			if d := entry.NextRun.Sub(a.clock.Now()); d < delay {
				delay = d
			}
		}
		if delay < time.Second {
			// A due or corrupt entry must not spin the scheduler at 100% CPU.
			delay = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-a.clock.After(delay):
			if err := a.refreshSchedule(ctx, &state); err != nil {
				a.logger.Printf("schedule refresh failed: %v", err)
			}
		}
	}
}

func (a *Agent) refreshSchedule(ctx context.Context, state *ScheduleState) error {
	connections, err := a.opts.Connections.List(ctx)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(connections))
	now := a.clock.Now().UTC()
	for _, connection := range connections {
		id, schedule := connection.ID, connection.Schedule
		if err := domain.ValidateSchedule(schedule); err != nil {
			a.logger.Printf("invalid schedule connection=%s", id)
			continue
		}
		interval, err := ParseSchedule(schedule)
		if err != nil {
			continue
		}
		if interval == 0 {
			delete(state.Entries, id)
			continue
		}
		seen[id] = true
		entry, exists := state.Entries[id]
		if !exists || entry.Schedule != schedule || entry.NextRun.IsZero() {
			next, err := nextRun(id, schedule, now)
			if err != nil {
				return err
			}
			state.Entries[id] = ScheduleEntry{Schedule: schedule, NextRun: next}
		}
	}
	for id := range state.Entries {
		if !seen[id] {
			delete(state.Entries, id)
		}
	}
	return saveScheduleState(a.statePath, *state)
}
