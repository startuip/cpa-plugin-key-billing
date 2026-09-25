package billing

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCallerScopeMatchesHostVectors pins the hash to values produced by
// CLIProxyAPI's own sdk/cliproxy/session.CallerScope. Enforcement reads the
// host-computed caller_scope while Key synchronization derives it locally. If
// these disagree, a plan bound in the UI never matches traffic. Regenerate the
// vectors with the host implementation.
func TestCallerScopeMatchesHostVectors(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "api key",
			input: "sk-test-key-0001",
			want:  "6df3f70e71486751d90152257b4a86ead883a8861692be5221363c2077dd540f",
		},
		{
			name:  "short principal",
			input: "hello",
			want:  "a7db0244a3a1996a99259fd55d99ca80f57f2d41f96bb93942cdd40f2fef4cd1",
		},
		{
			name:  "empty is not attributable",
			input: "",
			want:  "",
		},
		{
			name:  "surrounding whitespace is trimmed like the host does",
			input: "  sk-test-key-0001\t",
			want:  "6df3f70e71486751d90152257b4a86ead883a8861692be5221363c2077dd540f",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CallerScope(test.input); got != test.want {
				t.Fatalf("CallerScope(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestPreviewKeyDoesNotLeakShortKeys(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "short keys are fully masked", input: "sk-123", want: "******"},
		{name: "boundary length is fully masked", input: "123456789012", want: "************"},
		{name: "a short key shows a third", input: "1234567890123", want: "12…23"},
		{name: "a medium key shows a third", input: "sk-test-key-0001", want: "sk-…01"},
		{name: "long keys keep head and tail", input: "sk-test-key-0001-abcdefghijklmn", want: "sk-tes…klmn"},
		{name: "characters, not bytes", input: "密钥密钥密钥密钥密钥密钥密钥密钥", want: "密钥密…密钥"},
		{name: "a preview is already masked", input: "sk-tes…0001", want: "sk-tes…0001"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := PreviewKey(test.input); got != test.want {
				t.Fatalf("PreviewKey(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
	for count := 13; count <= 200; count++ {
		key := strings.Repeat("k", count)
		preview := PreviewKey(key)
		if shown := utf8.RuneCountInString(preview) - 1; shown > count/3 || shown < 2 {
			t.Fatalf("a %d-character key shows %d characters: %q", count, shown, preview)
		}
		if PreviewKey(preview) != preview {
			t.Fatalf("masking %q again changed it", preview)
		}
	}
}
