package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSettingsAPIRequiresTokenLoopbackHostAndSameOrigin(t *testing.T) {
	s := &settingsServer{token: "secret-token", host: "127.0.0.1:4321"}
	h := s.guard(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	cases := []struct {
		name, host, origin, auth, query string
		want                            int
	}{
		{"bearer", "127.0.0.1:4321", "", "Bearer secret-token", "", 204},
		{"query token (EventSource, images)", "127.0.0.1:4321", "", "", "?token=secret-token", 204},
		{"same origin", "127.0.0.1:4321", "http://127.0.0.1:4321", "Bearer secret-token", "", 204},
		{"no token", "127.0.0.1:4321", "", "", "", 401},
		{"wrong token", "127.0.0.1:4321", "", "Bearer secret-tokeN", "", 401},
		{"DNS rebinding", "evil.example:4321", "", "Bearer secret-token", "", 403},
		{"other loopback name", "localhost:4321", "", "Bearer secret-token", "", 403},
		{"cross-origin page", "127.0.0.1:4321", "https://evil.example", "Bearer secret-token", "", 403},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPatch, "http://"+c.host+"/api/config"+c.query, nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: status %d; want %d", c.name, w.Code, c.want)
		}
	}
}
