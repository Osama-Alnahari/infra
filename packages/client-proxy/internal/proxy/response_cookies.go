package proxy

import (
	"net"
	"net/http"
	"strings"
)

const etlaqSandboxDomain = "sandbox.etlaq.sa"

func isEtlaqSandboxHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.TrimSuffix(host, ".")

	return host == etlaqSandboxDomain || strings.HasSuffix(host, "."+etlaqSandboxDomain)
}

// normalizeEmbeddedPreviewCookies makes sandbox application cookies usable
// when the sandbox is embedded by Studio on a different schemeful site.
//
// The client proxy exclusively serves sandbox traffic. Deployed applications
// do not pass through this proxy and retain the cookie policy chosen by the
// application. Partitioned cookies remain isolated to the embedding top-level
// site while SameSite=None permits them to be sent from the embedded preview.
func normalizeEmbeddedPreviewCookies(response *http.Response) error {
	cookies := response.Header.Values("Set-Cookie")
	if len(cookies) == 0 {
		return nil
	}

	rewritten := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		rewritten = append(rewritten, normalizeEmbeddedPreviewCookie(cookie))
	}

	response.Header.Del("Set-Cookie")
	for _, cookie := range rewritten {
		response.Header.Add("Set-Cookie", cookie)
	}

	return nil
}

func normalizeEmbeddedPreviewCookie(cookie string) string {
	parts := strings.Split(cookie, ";")
	if len(parts) == 0 || !strings.Contains(parts[0], "=") {
		return cookie
	}

	normalized := make([]string, 0, len(parts)+3)
	normalized = append(normalized, strings.TrimSpace(parts[0]))

	for _, part := range parts[1:] {
		attribute := strings.TrimSpace(part)
		if attribute == "" {
			continue
		}

		name, _, _ := strings.Cut(attribute, "=")
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "domain", "secure", "samesite", "partitioned":
			continue
		default:
			normalized = append(normalized, attribute)
		}
	}

	normalized = append(normalized, "Secure", "SameSite=None", "Partitioned")
	return strings.Join(normalized, "; ")
}
