package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEndpointFormsResolveToChatAndModelURLs(t *testing.T) {
	for in, want := range map[string][2]string{
		"http://127.0.0.1:11434":                  {"http://127.0.0.1:11434/v1/chat/completions", "http://127.0.0.1:11434/v1/models"},
		"http://localhost:1234/v1/":               {"http://localhost:1234/v1/chat/completions", "http://localhost:1234/v1/models"},
		"https://api.example/v1/chat/completions": {"https://api.example/v1/chat/completions", "https://api.example/v1/models"},
		"https://gateway.example/openai/v1":       {"https://gateway.example/openai/v1/chat/completions", "https://gateway.example/openai/v1/models"},
	} {
		if got := ChatURL(in); got != want[0] {
			t.Errorf("ChatURL(%q) = %q", in, got)
		}
		if got := ModelsURL(in); got != want[1] {
			t.Errorf("ModelsURL(%q) = %q", in, got)
		}
	}
}

func TestEmptyModelUsesTheLargestLoadedModel(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[
				{"id":"tiny","status":"loaded","size":100},
				{"id":"huge-unloaded","status":{"value":"unloaded"},"size":900},
				{"id":"big","status":{"value":"loaded"},"size":500}]}`))
		case "/v1/chat/completions":
			var body struct{ Model string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			asked = body.Model
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Polished."}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL}
	models, err := c.Models(context.Background())
	if err != nil || len(models) != 3 || models[0].ID != "big" || models[2].ID != "huge-unloaded" {
		t.Fatalf("models %+v (%v)", models, err)
	}
	out, err := c.Complete(context.Background(), Request{System: "s", User: "u"})
	if err != nil || out != "Polished." || asked != "big" {
		t.Fatalf("complete %q via %q (%v)", out, asked, err)
	}
}

func TestCredentialsComeOnlyFromTheNamedVariable(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, Model: "m", APIKeyEnv: "VOICE_IO_LLM_TEST_KEY"}
	t.Setenv("VOICE_IO_LLM_TEST_KEY", "")
	if _, err := c.Complete(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "VOICE_IO_LLM_TEST_KEY") {
		t.Fatalf("unset key: %v", err)
	}
	if auth != "" {
		t.Fatal("request sent without the key")
	}
	t.Setenv("VOICE_IO_LLM_TEST_KEY", "sk-test")
	if _, err := c.Complete(context.Background(), Request{}); err != nil || auth != "Bearer sk-test" {
		t.Fatalf("auth %q (%v)", auth, err)
	}
}

func TestErrorsAreTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := (&Client{Endpoint: srv.URL, Model: "m"}).Complete(context.Background(), Request{})
	if he, ok := err.(*HTTPError); !ok || he.Code != 401 {
		t.Fatalf("HTTP error %v", err)
	}
	if _, err := (&Client{Endpoint: srv.URL}).Complete(context.Background(), Request{}); err == nil ||
		!strings.Contains(err.Error(), "no models") {
		t.Fatalf("empty model list: %v", err)
	}
}
