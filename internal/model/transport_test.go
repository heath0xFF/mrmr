package model

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestModelRejectsRedirects(t *testing.T) {
	const key = "synthetic-private-model-key"
	t.Setenv("MRMR_REDIRECT_TEST_KEY", key)
	for _, provider := range []string{"openai-compatible", "liquid-d1"} {
		t.Run(provider, func(t *testing.T) {
			var forwarded atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/other" {
					forwarded.Add(1)
					return
				}
				http.Redirect(w, r, "/other", http.StatusTemporaryRedirect)
			}))
			defer srv.Close()
			cfg := Config{Provider: provider, BaseURL: srv.URL, Model: "test-model", APIKeyEnv: "MRMR_REDIRECT_TEST_KEY"}
			_, _, _, err := (&Client{}).InterpretWithQuestions(context.Background(), cfg, "judge", "", nil, testTypeSafeQuestions(), []byte(`{"message":"synthetic"}`))
			if err == nil || forwarded.Load() != 0 || strings.Contains(err.Error(), key) {
				t.Fatalf("redirect err=%v forwarded=%d", err, forwarded.Load())
			}
		})
	}
}

func TestOpenAIResponseSizeIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"`+strings.Repeat("x", 4<<20)+`"}}]}`)
	}))
	defer srv.Close()
	cfg := Config{Provider: "openai-compatible", BaseURL: srv.URL, Model: "test-model"}
	_, _, _, err := (&Client{}).InterpretWithQuestions(context.Background(), cfg, "judge", "", nil, testTypeSafeQuestions(), []byte(`{}`))
	if err == nil {
		t.Fatal("oversized model response accepted")
	}
}
