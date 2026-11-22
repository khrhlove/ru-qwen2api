package main

import (
	"context"
	"github.com/yuanjie-ai/qwen2api/internal/panel"
	"net/http"
)

type offlineBackend struct{ panel.Backend }

func (offlineBackend) RefreshCredits(context.Context, bool) {}

// Offline mode mounts only local panel operations. The normal gateway handler
// starts a video watcher, so it must not be constructed in this mode.
func offlinePanel(panel http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.Redirect(w, r, "/panel/", http.StatusFound)
		case "/healthz", "/readyz":
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/readyz" {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			_, _ = w.Write([]byte(`{"status":"offline","accounts_ready":0}`))
		case "/panel/", "/panel/favicon.svg", "/panel/api/session", "/panel/api/login", "/panel/api/logout",
			"/panel/api/overview", "/panel/api/accounts", "/panel/api/keys", "/panel/api/config", "/panel/api/requests", "/panel/api/runlog":
			if r.Method == http.MethodGet || r.URL.Path == "/panel/api/login" || r.URL.Path == "/panel/api/logout" {
				panel.ServeHTTP(w, r)
				return
			}
			fallthrough
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"Автономный режим: внешний сервис Qwen отключён"}`))
		}
	})
}
