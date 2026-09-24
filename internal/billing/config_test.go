package billing

import "testing"

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
