package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOfflineBlocksUpstreamActions(t *testing.T) {
	h := offlinePanel(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("offline operation reached the live panel")
	}))
	for _, path := range []string{"/panel/api/debug/v1/chat/completions", "/panel/api/accounts/import", "/panel/api/credits/refresh", "/panel/api/models", "/v1/chat/completions"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		if w.Code != http.StatusServiceUnavailable { t.Errorf("%s: %d", path,w.Code) }
	}
}
