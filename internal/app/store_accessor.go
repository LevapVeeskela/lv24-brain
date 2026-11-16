package app

import "github.com/levapveeskela/lv24-brain/internal/ports"

// Store returns the underlying store dependency.
func (a *App) Store() ports.Store {
	return a.deps.Store
}
