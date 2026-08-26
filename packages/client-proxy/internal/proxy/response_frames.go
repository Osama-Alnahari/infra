package proxy

import (
	"net/http"
	"strings"
)

const studioFrameAncestors = "frame-ancestors https://etlaq.sa https://www.etlaq.sa https://dev.etlaq.sa https://www.dev.etlaq.sa https://*.run.app http://localhost:* http://127.0.0.1:*"

// normalizeEmbeddedPreviewFrames allows sandbox applications to be embedded
// only by Etlaq Studio, Cloud Run environments, and local development. Old generated projects
// may emit X-Frame-Options: SAMEORIGIN or their own frame-ancestors directive;
// both otherwise prevent the cross-origin preview iframe from loading.
func normalizeEmbeddedPreviewFrames(response *http.Response) error {
	response.Header.Del("X-Frame-Options")
	normalizeFrameAncestors(response.Header, "Content-Security-Policy")
	normalizeFrameAncestors(response.Header, "Content-Security-Policy-Report-Only")

	return nil
}

func normalizeFrameAncestors(header http.Header, name string) {
	policies := header.Values(name)
	if len(policies) == 0 {
		if name == "Content-Security-Policy" {
			header.Set(name, studioFrameAncestors)
		}
		return
	}

	header.Del(name)
	for _, policy := range policies {
		directives := strings.Split(policy, ";")
		rewritten := make([]string, 0, len(directives)+1)
		for _, directive := range directives {
			directive = strings.TrimSpace(directive)
			if directive == "" || isFrameAncestorsDirective(directive) {
				continue
			}
			rewritten = append(rewritten, directive)
		}
		rewritten = append(rewritten, studioFrameAncestors)
		header.Add(name, strings.Join(rewritten, "; "))
	}
}

func isFrameAncestorsDirective(directive string) bool {
	fields := strings.Fields(directive)
	return len(fields) > 0 && strings.EqualFold(fields[0], "frame-ancestors")
}

func normalizeEmbeddedPreviewResponse(response *http.Response) error {
	if err := normalizeEmbeddedPreviewCookies(response); err != nil {
		return err
	}

	return normalizeEmbeddedPreviewFrames(response)
}
