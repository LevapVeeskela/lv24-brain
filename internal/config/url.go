package config

import (
	"net/url"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func validateServerURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return domain.NewValidationError("config.server_url", "is not a valid URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return domain.NewValidationError("config.server_url", "must use http or https")
	}
	if parsed.Host == "" {
		return domain.NewValidationError("config.server_url", "must include a host")
	}
	return nil
}
