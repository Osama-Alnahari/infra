package proxy

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeEmbeddedPreviewFramesReplacesBlockingHeaders(t *testing.T) {
	t.Parallel()

	response := &http.Response{Header: make(http.Header)}
	response.Header.Set("X-Frame-Options", "SAMEORIGIN")
	response.Header.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'self'; script-src 'self' 'unsafe-inline'")

	require.NoError(t, normalizeEmbeddedPreviewFrames(response))
	assert.Empty(t, response.Header.Values("X-Frame-Options"))
	assert.Equal(t,
		"default-src 'self'; script-src 'self' 'unsafe-inline'; "+studioFrameAncestors,
		response.Header.Get("Content-Security-Policy"),
	)
}

func TestNormalizeEmbeddedPreviewFramesAddsPolicyWhenMissing(t *testing.T) {
	t.Parallel()

	response := &http.Response{Header: make(http.Header)}
	require.NoError(t, normalizeEmbeddedPreviewFrames(response))
	assert.Equal(t, studioFrameAncestors, response.Header.Get("Content-Security-Policy"))
}

func TestNormalizeEmbeddedPreviewFramesPreservesMultiplePolicies(t *testing.T) {
	t.Parallel()

	response := &http.Response{Header: make(http.Header)}
	response.Header.Add("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
	response.Header.Add("Content-Security-Policy", "img-src https: data:")

	require.NoError(t, normalizeEmbeddedPreviewFrames(response))
	assert.Equal(t, []string{
		"default-src 'self'; " + studioFrameAncestors,
		"img-src https: data:; " + studioFrameAncestors,
	}, response.Header.Values("Content-Security-Policy"))
}

func TestNormalizeEmbeddedPreviewResponseNormalizesCookiesAndFrames(t *testing.T) {
	t.Parallel()

	response := &http.Response{Header: make(http.Header)}
	response.Header.Set("X-Frame-Options", "DENY")
	response.Header.Add("Set-Cookie", "session=opaque; Path=/; SameSite=Lax")

	require.NoError(t, normalizeEmbeddedPreviewResponse(response))
	assert.Empty(t, response.Header.Values("X-Frame-Options"))
	assert.Equal(t, studioFrameAncestors, response.Header.Get("Content-Security-Policy"))
	assert.Equal(t, "session=opaque; Path=/; Secure; SameSite=None; Partitioned", response.Header.Get("Set-Cookie"))
}
