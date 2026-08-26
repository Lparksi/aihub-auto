// Package configuration parses the non-secret configuration owned by the plugin.
package configuration

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultProviderID             = "aihub-auto"
	DefaultBaseURL                = "https://aihub.top"
	DefaultMaxRequestBytes  int64 = 4 * 1024 * 1024
	DefaultMaxResponseBytes int64 = 4 * 1024 * 1024
	DefaultPoolSize               = 4
	DefaultSessionTTL             = 24 * time.Hour
)

// Config contains phase-one settings. Credentials are deliberately excluded:
// CPA retains credentials in its auth store rather than plugin configuration.
type Config struct {
	ProviderID        string        `yaml:"provider_id"`
	BaseURL           string        `yaml:"base_url"`
	MaxResponseBytes  int64         `yaml:"max_response_bytes"`
	MaxRequestBytes   int64         `yaml:"max_request_bytes"`
	ManagementEnabled bool          `yaml:"management_enabled"`
	Mode              string        `yaml:"mode"`
	PriceBandMin      float64       `yaml:"price_band_min"`
	PriceBandMax      float64       `yaml:"price_band_max"`
	ManualLockAuthID  string        `yaml:"manual_lock_auth_id"`
	ModelPatterns     []string      `yaml:"model_patterns"`
	StateDir          string        `yaml:"state_dir"`
	DefaultPoolSize   int           `yaml:"default_pool_size"`
	LunaPoolSize      int           `yaml:"luna_pool_size"`
	SessionTTL        time.Duration `yaml:"session_ttl"`
}

// rawConfig includes the fields CPA owns as well as this plugin's settings.
// CPA uses enabled and priority to control plugin activation and ordering; the
// plugin must accept but never act on them.
type rawConfig struct {
	Enabled           bool          `yaml:"enabled"`
	Priority          int           `yaml:"priority"`
	ProviderID        string        `yaml:"provider_id"`
	BaseURL           string        `yaml:"base_url"`
	Mode              string        `yaml:"mode"`
	PriceBandMin      float64       `yaml:"price_band_min"`
	PriceBandMax      float64       `yaml:"price_band_max"`
	ManualLockAuthID  string        `yaml:"manual_lock_auth_id"`
	ModelPatterns     []string      `yaml:"model_patterns"`
	MaxResponseBytes  int64         `yaml:"max_response_bytes"`
	MaxRequestBytes   int64         `yaml:"max_request_bytes"`
	ManagementEnabled bool          `yaml:"management_enabled"`
	StateDir          string        `yaml:"state_dir"`
	DefaultPoolSize   int           `yaml:"default_pool_size"`
	LunaPoolSize      int           `yaml:"luna_pool_size"`
	SessionTTL        time.Duration `yaml:"session_ttl"`
}

// Defaults returns a safe initial configuration.
func Defaults() Config {
	return Config{
		ProviderID:        DefaultProviderID,
		BaseURL:           DefaultBaseURL,
		MaxResponseBytes:  DefaultMaxResponseBytes,
		MaxRequestBytes:   DefaultMaxRequestBytes,
		ManagementEnabled: true,
		Mode:              "balanced",
		PriceBandMin:      0,
		PriceBandMax:      1,
		DefaultPoolSize:   DefaultPoolSize,
		LunaPoolSize:      DefaultPoolSize,
		SessionTTL:        DefaultSessionTTL,
	}
}

// Parse decodes the host-provided YAML, supplies omitted values, and validates
// values that influence the plugin's externally visible behavior.
func Parse(rawYAML []byte) (Config, error) {
	configuration := Defaults()
	if len(rawYAML) == 0 {
		return configuration, nil
	}
	decodedConfiguration := rawConfig{
		ProviderID:        configuration.ProviderID,
		BaseURL:           configuration.BaseURL,
		MaxResponseBytes:  configuration.MaxResponseBytes,
		MaxRequestBytes:   configuration.MaxRequestBytes,
		ManagementEnabled: configuration.ManagementEnabled,
		Mode:              configuration.Mode,
		PriceBandMin:      configuration.PriceBandMin,
		PriceBandMax:      configuration.PriceBandMax,
		DefaultPoolSize:   configuration.DefaultPoolSize,
		LunaPoolSize:      configuration.LunaPoolSize,
		SessionTTL:        configuration.SessionTTL,
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(rawYAML)))
	decoder.KnownFields(true)
	if errDecode := decoder.Decode(&decodedConfiguration); errDecode != nil {
		return Config{}, fmt.Errorf("decode plugin configuration: %w", errDecode)
	}
	configuration.ProviderID = decodedConfiguration.ProviderID
	configuration.BaseURL = decodedConfiguration.BaseURL
	configuration.MaxResponseBytes = decodedConfiguration.MaxResponseBytes
	configuration.MaxRequestBytes = decodedConfiguration.MaxRequestBytes
	configuration.ManagementEnabled = decodedConfiguration.ManagementEnabled
	configuration.Mode = strings.ToLower(strings.TrimSpace(decodedConfiguration.Mode))
	configuration.PriceBandMin = decodedConfiguration.PriceBandMin
	configuration.PriceBandMax = decodedConfiguration.PriceBandMax
	configuration.ManualLockAuthID = strings.TrimSpace(decodedConfiguration.ManualLockAuthID)
	configuration.ModelPatterns = normalizeModelPatterns(decodedConfiguration.ModelPatterns)
	configuration.StateDir = strings.TrimSpace(decodedConfiguration.StateDir)
	configuration.DefaultPoolSize = decodedConfiguration.DefaultPoolSize
	configuration.LunaPoolSize = decodedConfiguration.LunaPoolSize
	configuration.SessionTTL = decodedConfiguration.SessionTTL

	configuration.ProviderID = strings.TrimSpace(configuration.ProviderID)
	if configuration.ProviderID == "" {
		configuration.ProviderID = DefaultProviderID
	}
	baseURL, errBaseURL := normalizeBaseURL(configuration.BaseURL)
	if errBaseURL != nil {
		return Config{}, errBaseURL
	}
	configuration.BaseURL = baseURL
	if configuration.MaxResponseBytes <= 0 {
		return Config{}, fmt.Errorf("max_response_bytes must be greater than zero")
	}
	if configuration.MaxRequestBytes <= 0 {
		return Config{}, fmt.Errorf("max_request_bytes must be greater than zero")
	}
	if configuration.Mode != "economy" && configuration.Mode != "balanced" && configuration.Mode != "speed" {
		return Config{}, fmt.Errorf("mode must be economy, balanced, or speed")
	}
	if configuration.PriceBandMin < 0 || configuration.PriceBandMax < configuration.PriceBandMin {
		return Config{}, fmt.Errorf("price_band_min and price_band_max must define a non-negative inclusive range")
	}
	if configuration.DefaultPoolSize <= 0 || configuration.LunaPoolSize <= 0 {
		return Config{}, fmt.Errorf("default_pool_size and luna_pool_size must be greater than zero")
	}
	if configuration.SessionTTL <= 0 {
		return Config{}, fmt.Errorf("session_ttl must be greater than zero")
	}
	if configuration.StateDir != "" && !filepath.IsAbs(configuration.StateDir) {
		return Config{}, fmt.Errorf("state_dir must be an absolute path")
	}
	return configuration, nil
}

func normalizeModelPatterns(patterns []string) []string {
	normalized := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern != "" {
			normalized = append(normalized, pattern)
		}
	}
	return normalized
}

// normalizeBaseURL accepts only a complete HTTP(S) origin. Keeping the base
// URL origin-only prevents credentials from being sent to a path, query, or
// userinfo supplied accidentally in plugin configuration.
func normalizeBaseURL(rawBaseURL string) (string, error) {
	trimmedBaseURL := strings.TrimSpace(rawBaseURL)
	if trimmedBaseURL == "" {
		return DefaultBaseURL, nil
	}
	parsedURL, errParse := url.Parse(trimmedBaseURL)
	if errParse != nil || parsedURL == nil {
		return "", fmt.Errorf("base_url must be a valid HTTP(S) origin")
	}
	if (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" || parsedURL.Path != "" && parsedURL.Path != "/" {
		return "", fmt.Errorf("base_url must be an HTTP(S) origin without path, credentials, query, or fragment")
	}
	return strings.TrimRight(parsedURL.String(), "/"), nil
}
