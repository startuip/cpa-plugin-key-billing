package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// callerScopeSalt must stay byte-identical to CLIProxyAPI's
// sdk/cliproxy/session.CallerScope. The host derives caller_scope with this
// exact prefix and places it in interceptor metadata; this plugin derives the
// same value when the admin UI sends the configured key list. Changing it
// silently detaches those synchronized keys from request accounting, so a
// fixed host-generated vector covers it.
const callerScopeSalt = "cli-proxy-api:caller-scope:v1\x00"

const UnknownKeyPreview = "unknown"

// Blank principals are not attributable and therefore have no scope.
func CallerScope(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(callerScopeSalt + value))
	return hex.EncodeToString(sum[:])
}

// PreviewKey masks a plaintext key for display and persistence. The stored
// caller scope is a salted hash anyone can recompute, so a preview reveals at
// most a third of a key: six leading and four trailing characters only from 30
// characters on. Masked input is returned unchanged. The UI mirrors this rule.
func PreviewKey(key string) string {
	key = strings.TrimSpace(key)
	runes := []rune(key)
	count := len(runes)
	switch {
	case count == 0:
		return ""
	case isKeyPreview(key):
		return key
	case count <= 12:
		// Too short to mask meaningfully without leaking most of it.
		return strings.Repeat("*", count)
	}
	suffix := min(4, count/6)
	prefix := min(6, count/3-suffix)
	return string(runes[:prefix]) + "…" + string(runes[count-suffix:])
}

func isKeyPreview(value string) bool {
	prefix, suffix, found := strings.Cut(value, "…")
	prefixCount, suffixCount := utf8.RuneCountInString(prefix), utf8.RuneCountInString(suffix)
	return found && !strings.Contains(suffix, "…") && prefixCount >= 1 && prefixCount <= 6 && suffixCount >= 1 && suffixCount <= 4
}

func freeID(name, prefix string, taken func(string) bool) string {
	// Names without Latin letters use numbered IDs rather than numeric fragments.
	if base := slugify(name); strings.ContainsAny(base, "abcdefghijklmnopqrstuvwxyz") {
		if !taken(base) {
			return base
		}
		for i := 2; i < 1000; i++ {
			if candidate := fmt.Sprintf("%s-%d", base, i); !taken(candidate) {
				return candidate
			}
		}
	}
	for i := 1; ; i++ {
		if candidate := fmt.Sprintf("%s-%d", prefix, i); !taken(candidate) {
			return candidate
		}
	}
}

func slugify(value string) string {
	var builder strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen && builder.Len() > 0 {
				builder.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if len(slug) > 48 {
		slug = strings.Trim(slug[:48], "-")
	}
	return slug
}
