package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func TestLocalPanelNeedsNoCredentials(t *testing.T) {
	p, _ := withPanel(func(c *config.Config) {
		c.Panel.LocalNoAuth = true
		c.APIKeys = []string{"api-key-still-required"}
	})
	for _, path := range []string{"/panel/", "/panel/api/overview", "/panel/api/session"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8400"+path, nil)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if path == "/panel/api/session" {
			var s struct {
				Authed   bool   `json:"authed"`
				NeedAuth bool   `json:"need_auth"`
				Method   string `json:"method"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
				t.Fatal(err)
			}
			if !s.Authed || s.NeedAuth || s.Method != "local" {
				t.Fatalf("unexpected session: %+v", s)
			}
		}
	}
}

func TestLocalPanelOriginGuards(t *testing.T) {
	for _, tc := range []struct {
		name, method, host, origin, site string
		want                             bool
	}{
		{"loopback", "GET", "127.0.0.1:8400", "", "", true},
		{"localhost", "GET", "localhost:8400", "", "", true},
		{"IPv6", "GET", "[::1]:8400", "", "", true},
		{"same-origin write", "POST", "127.0.0.1:8400", "http://127.0.0.1:8400", "same-origin", true},
		{"remote host", "GET", "192.168.0.20:8400", "", "", false},
		{"rebound host", "GET", "evil.example:8400", "", "", false},
		{"cross-origin", "POST", "127.0.0.1:8400", "https://evil.example", "", false},
		{"other local port", "POST", "127.0.0.1:8400", "http://127.0.0.1:8083", "", false},
		{"no origin write", "POST", "127.0.0.1:8400", "", "", false},
		{"null origin", "GET", "127.0.0.1:8400", "null", "", false},
		{"cross-site get", "GET", "127.0.0.1:8400", "", "cross-site", false},
		{"same-site get", "GET", "127.0.0.1:8400", "", "same-site", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := withPanel(func(c *config.Config) { c.Panel.LocalNoAuth = true })
			r := httptest.NewRequest(tc.method, "http://"+tc.host+"/panel/api/session", nil)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			r.Header.Set("X-Forwarded-For", "127.0.0.1")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if (w.Code == http.StatusOK) != tc.want {
				t.Fatalf("unexpected status: %d", w.Code)
			}
		})
	}
}
