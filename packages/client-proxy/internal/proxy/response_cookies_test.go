package proxy

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeEmbeddedPreviewCookie(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "replaces lax policy and preserves application attributes",
			input:    "gh_session=opaque; Path=/; Expires=Mon, 07 Sep 2026 21:57:52 GMT; Max-Age=2592000; HttpOnly; SameSite=Lax",
			expected: "gh_session=opaque; Path=/; Expires=Mon, 07 Sep 2026 21:57:52 GMT; Max-Age=2592000; HttpOnly; Secure; SameSite=None; Partitioned",
		},
		{
			name:     "normalizes an existing cross-site policy without duplication",
			input:    "session=opaque; Domain=.sandbox.etlaq.sa; secure; SameSite=Strict; PARTITIONED; Priority=High",
			expected: "session=opaque; Priority=High; Secure; SameSite=None; Partitioned",
		},
		{
			name:     "preserves deletion semantics",
			input:    "session=; Path=/; Max-Age=0; HttpOnly",
			expected: "session=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=None; Partitioned",
		},
		{
			name:     "leaves malformed header unchanged",
			input:    "not-a-cookie",
			expected: "not-a-cookie",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.expected, normalizeEmbeddedPreviewCookie(test.input))
		})
	}
}

func TestIsEtlaqSandboxHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		host     string
		expected bool
	}{
		{host: "3000-i43n3y14sqjqjz4v4k1s4.sandbox.etlaq.sa", expected: true},
		{host: "3000-i43n3y14sqjqjz4v4k1s4.SANDBOX.ETLAQ.SA:443", expected: true},
		{host: "sandbox.etlaq.sa", expected: true},
		{host: "sandbox.etlaq.sa.", expected: true},
		{host: "sandbox.etlaq.sa.evil.example", expected: false},
		{host: "notsandbox.etlaq.sa", expected: false},
		{host: "example.com", expected: false},
	}

	for _, test := range tests {
		assert.Equal(t, test.expected, isEtlaqSandboxHost(test.host), test.host)
	}
}

func TestNormalizeEmbeddedPreviewCookiesPreservesMultipleHeaders(t *testing.T) {
	t.Parallel()

	response := &http.Response{Header: make(http.Header)}
	response.Header.Add("Set-Cookie", "session=one; Path=/; HttpOnly; SameSite=Lax")
	response.Header.Add("Set-Cookie", "csrf=two; Path=/auth; SameSite=Strict")

	require.NoError(t, normalizeEmbeddedPreviewCookies(response))
	assert.Equal(t, []string{
		"session=one; Path=/; HttpOnly; Secure; SameSite=None; Partitioned",
		"csrf=two; Path=/auth; Secure; SameSite=None; Partitioned",
	}, response.Header.Values("Set-Cookie"))
}

func TestNormalizeEmbeddedPreviewCookiesDoesNothingWithoutCookies(t *testing.T) {
	t.Parallel()

	response := &http.Response{Header: make(http.Header)}
	require.NoError(t, normalizeEmbeddedPreviewCookies(response))
	assert.Empty(t, response.Header.Values("Set-Cookie"))
}
