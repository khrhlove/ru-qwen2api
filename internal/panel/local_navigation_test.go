package panel

import (
	"net/http/httptest"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func TestLocalPanelNavigationFromGemini(t *testing.T) {
	for _, tc := range []struct {
		name, method, host, path, site, mode, dest string
		status                                     int
	}{
		{"Gemini sidebar", "GET", "127.0.0.1:8400", "/panel/", "same-site", "navigate", "document", 200},
		{"Firefox extension", "GET", "127.0.0.1:8400", "/panel/", "cross-site", "navigate", "document", 200},
		{"no trailing slash", "GET", "localhost:8400", "/panel", "same-site", "navigate", "document", 200},
		{"remote Host", "GET", "evil.example:8400", "/panel/", "same-site", "navigate", "document", 403},
		{"API navigation", "GET", "127.0.0.1:8400", "/panel/api/overview", "same-site", "navigate", "document", 403},
		{"cross-site fetch", "GET", "127.0.0.1:8400", "/panel/", "cross-site", "cors", "empty", 403},
		{"embedded panel", "GET", "127.0.0.1:8400", "/panel/", "cross-site", "navigate", "iframe", 403},
		{"form submit", "POST", "127.0.0.1:8400", "/panel/", "same-site", "navigate", "document", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := withPanel(func(c *config.Config) { c.Panel.LocalNoAuth = true })
			r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			r.Header.Set("Sec-Fetch-Mode", tc.mode)
			r.Header.Set("Sec-Fetch-Dest", tc.dest)
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
		})
	}
}
