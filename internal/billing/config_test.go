package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDecodeConfigDefaults(t *testing.T) {
	cfg, errDecode := DecodeConfig([]byte("enabled: true\npriority: 10\nstore:\n  id: cpa-key-billing\n  version: 0.5.1\n"))
	if errDecode != nil {
		t.Fatalf("DecodeConfig: %v", errDecode)
	}
	if !cfg.Enabled || cfg.Debug || cfg.CodexFastModeBilling || cfg.MaskAPIKeyViewEmails || cfg.AllowAPIKeyQuotaReset || cfg.PauseResetFollowSync || cfg.StateFile != DefaultStateFile {
		t.Fatalf("config = %+v", cfg)
	}
	cfg, errDecode = DecodeConfig([]byte("enabled: true\ndebug: true\ncodex_fast_mode_billing: true\nmask_api_key_view_emails: true\nallow_api_key_quota_reset: true\npause_reset_follow_sync: true\n"))
	if errDecode != nil || !cfg.Debug || !cfg.CodexFastModeBilling || !cfg.MaskAPIKeyViewEmails || !cfg.AllowAPIKeyQuotaReset || !cfg.PauseResetFollowSync {
		t.Fatalf("config = %+v, error = %v", cfg, errDecode)
	}
}

func TestDecodeConfigRejectsUnknownFieldsAndExtraDocuments(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field":  "enable: true\n",
		"extra document": "enabled: true\n---\nenabled: false\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, errDecode := DecodeConfig([]byte(raw)); errDecode == nil {
				t.Fatal("DecodeConfig accepted invalid configuration")
			}
		})
	}
}

func TestReferencePriceProxySetting(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"", true}, {"direct", true}, {"NONE", true},
		{"http://proxy.example:8080", true}, {"socks5h://dummy-user:dummy-secret@proxy.example:1080", true},
		{"ftp://proxy.example", false}, {"proxy.example:8080", false}, {"http://dummy-user:dummy-secret@", false},
	} {
		_, err := DecodeConfig([]byte("reference_price_proxy: " + strconv.Quote(test.value) + "\n"))
		if (err == nil) != test.valid {
			t.Fatalf("%q: error = %v", test.value, err)
		}
		if err != nil && strings.Contains(err.Error(), "dummy-secret") {
			t.Fatalf("proxy credentials leaked: %v", err)
		}
	}
	if proxy, _ := referencePriceProxy("direct"); proxy != nil {
		t.Fatal("direct still uses a proxy")
	}
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect && r.Host == "models.dev:443" {
			connects.Add(1)
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(proxy.Close)
	if _, err := downloadModelsDevPrices(context.Background(), proxy.URL); err == nil || connects.Load() != 1 {
		t.Fatalf("download did not use the configured proxy: err = %v, connects = %d", err, connects.Load())
	}
}
