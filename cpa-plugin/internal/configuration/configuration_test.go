package configuration

import "testing"

func TestParseAppliesDefaultsAndRoutingMode(t *testing.T) {
	configuration, errParse := Parse([]byte("enabled: true\npriority: 7\nmode:  balanced \nmanagement_enabled: true\n"))
	if errParse != nil {
		t.Fatalf("Parse() error = %v", errParse)
	}
	if configuration.MaxResponseBytes != DefaultMaxResponseBytes {
		t.Fatalf("MaxResponseBytes = %d, want %d", configuration.MaxResponseBytes, DefaultMaxResponseBytes)
	}
	if configuration.ProviderID != DefaultProviderID {
		t.Fatalf("ProviderID = %q, want default %q", configuration.ProviderID, DefaultProviderID)
	}
	if !configuration.ManagementEnabled {
		t.Fatalf("configuration flags = %#v, want management enabled", configuration)
	}
	if configuration.Mode != "balanced" || configuration.PriceBandMin != 0 || configuration.PriceBandMax != 1 {
		t.Fatalf("routing defaults = %#v, want balanced 0..1 band", configuration)
	}
}

func TestParseConfiguresImplementedRoutingFields(t *testing.T) {
	configuration, errParse := Parse([]byte("mode: speed\nprice_band_min: 0.1\nprice_band_max: 0.5\nmanual_lock_auth_id: locked-auth\nmodel_patterns: [' GPT-* ', 'o3']\n"))
	if errParse != nil {
		t.Fatalf("Parse() error = %v", errParse)
	}
	if configuration.Mode != "speed" || configuration.PriceBandMin != .1 || configuration.PriceBandMax != .5 || configuration.ManualLockAuthID != "locked-auth" || len(configuration.ModelPatterns) != 2 || configuration.ModelPatterns[0] != "gpt-*" {
		t.Fatalf("routing configuration = %#v", configuration)
	}
}

func TestParseConfiguresPhaseFourPoolAndSessionSettings(t *testing.T) {
	configuration, errParse := Parse([]byte("state_dir: /var/lib/aihub-auto\ndefault_pool_size: 3\nluna_pool_size: 2\nsession_ttl: 2h\n"))
	if errParse != nil {
		t.Fatalf("Parse() error = %v", errParse)
	}
	if configuration.StateDir != "/var/lib/aihub-auto" || configuration.DefaultPoolSize != 3 || configuration.LunaPoolSize != 2 || configuration.SessionTTL.String() != "2h0m0s" {
		t.Fatalf("phase four configuration = %#v", configuration)
	}
}

func TestParseRejectsInvalidRoutingFields(t *testing.T) {
	for _, raw := range []string{"mode: fastest", "price_band_min: 1\nprice_band_max: 0"} {
		if _, errParse := Parse([]byte(raw)); errParse == nil {
			t.Fatalf("Parse(%q) error = nil, want routing validation", raw)
		}
	}
}

func TestParseAcceptsCPAOwnedFieldsAndQuotedHashValues(t *testing.T) {
	configuration, errParse := Parse([]byte("enabled: false\npriority: 10\nprovider_id: \"aihub#auto\"\nmode: speed\n"))
	if errParse != nil {
		t.Fatalf("Parse() error = %v", errParse)
	}
	if configuration.ProviderID != "aihub#auto" {
		t.Fatalf("ProviderID = %q, want quoted hash value", configuration.ProviderID)
	}
}

func TestParseRejectsInvalidResponseLimit(t *testing.T) {
	_, errParse := Parse([]byte("max_response_bytes: 0"))
	if errParse == nil {
		t.Fatal("Parse() error = nil, want validation error")
	}
}

func TestParseRejectsUnknownPluginField(t *testing.T) {
	_, errParse := Parse([]byte("priority: 1\nunsupported_plugin_setting: true\n"))
	if errParse == nil {
		t.Fatal("Parse() error = nil, want unknown-field validation error")
	}
}

func TestParseNormalizesBaseURL(t *testing.T) {
	configuration, errParse := Parse([]byte("base_url: ' https://aihub.example/ '\n"))
	if errParse != nil {
		t.Fatalf("Parse() error = %v", errParse)
	}
	if configuration.BaseURL != "https://aihub.example" {
		t.Fatalf("BaseURL = %q, want normalized origin", configuration.BaseURL)
	}
}

func TestParseRejectsUnsafeBaseURLs(t *testing.T) {
	for _, baseURL := range []string{
		"ftp://aihub.example",
		"https://user:password@aihub.example",
		"https://aihub.example/api",
		"https://aihub.example?access_token=secret",
		"https://aihub.example#fragment",
	} {
		if _, errParse := Parse([]byte("base_url: " + baseURL + "\n")); errParse == nil {
			t.Fatalf("Parse(%q) error = nil, want validation error", baseURL)
		}
	}
}
