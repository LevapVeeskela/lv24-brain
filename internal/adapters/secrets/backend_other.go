//go:build !windows && !darwin && !linux

package secrets

import "context"

// platformBackend has no native secret store on this platform. Environment
// variables remain available; Set/Delete/Get fail closed with ErrUnsupported.
type platformBackend struct{}

func newPlatformBackend() backend {
	return platformBackend{}
}

func (platformBackend) Get(ctx context.Context, key string) (string, error) {
	return "", unavailable("no platform secret store for this operating system")
}

func (platformBackend) Set(ctx context.Context, key, value string) error {
	return unavailable("no platform secret store for this operating system")
}

func (platformBackend) Delete(ctx context.Context, key string) error {
	return unavailable("no platform secret store for this operating system")
}

func (platformBackend) Names(ctx context.Context) ([]string, error) {
	return nil, unavailable("no platform secret store for this operating system")
}
