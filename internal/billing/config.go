package billing

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultStateFile = "plugins/cpa-key-billing-state-v1.db"

type Config struct {
	Enabled               bool   `yaml:"enabled"`
	Debug                 bool   `yaml:"debug"`
	StateFile             string `yaml:"state_file"`
	CodexFastModeBilling  bool   `yaml:"codex_fast_mode_billing"`
	MaskAPIKeyViewEmails  bool   `yaml:"mask_api_key_view_emails"`
	AllowAPIKeyQuotaReset bool   `yaml:"allow_api_key_quota_reset"`
	PauseResetFollowSync  bool   `yaml:"pause_reset_follow_sync"`
	// ReferencePriceProxy routes models.dev downloads: empty uses the process
	// environment (HTTPS_PROXY and friends), "direct" or "none" connects
	// directly, and a URL uses that HTTP, HTTPS, SOCKS5 or SOCKS5H proxy. The
	// plugin never sees CPA's own proxy-url, so it has to be set here.
	ReferencePriceProxy string `yaml:"reference_price_proxy"`
}

func DefaultConfig() Config {
	return Config{
		Enabled:   false,
		StateFile: DefaultStateFile,
	}
}

func DecodeConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(bytes.TrimSpace(raw)) > 0 {
		document := struct {
			Config `yaml:",inline"`
			// These fields belong to the host and are ignored by the plugin.
			Priority int       `yaml:"priority"`
			Store    yaml.Node `yaml:"store"`
		}{Config: cfg}
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if errDecode := decoder.Decode(&document); errDecode != nil {
			return Config{}, fmt.Errorf("Parse plugin configuration: %w", errDecode)
		}
		if errTrailing := decoder.Decode(&struct{}{}); errTrailing != io.EOF {
			return Config{}, fmt.Errorf("Plugin configuration must contain exactly one YAML document")
		}
		cfg = document.Config
	}
	cfg = cfg.normalized()
	if _, err := referencePriceProxy(cfg.ReferencePriceProxy); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// referencePriceProxy never quotes the value: it may carry proxy credentials.
func referencePriceProxy(value string) (func(*http.Request) (*url.URL, error), error) {
	switch {
	case value == "":
		return http.ProxyFromEnvironment, nil
	case strings.EqualFold(value, "direct") || strings.EqualFold(value, "none"):
		return nil, nil
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.Hostname() != "" {
		switch parsed.Scheme {
		case "http", "https", "socks5", "socks5h":
			return http.ProxyURL(parsed), nil
		}
	}
	return nil, fmt.Errorf("Invalid reference_price_proxy; use an HTTP, HTTPS, SOCKS5, or SOCKS5H URL, direct, or none")
}

func (c Config) describe() string {
	if c.Enabled {
		return "enabled"
	}
	return "disabled"
}

func (c Config) normalized() Config {
	c.StateFile = strings.TrimSpace(c.StateFile)
	c.ReferencePriceProxy = strings.TrimSpace(c.ReferencePriceProxy)
	if c.StateFile == "" {
		c.StateFile = DefaultStateFile
	}
	return c
}
