package openaiapi

import (
	"context"
	"net/http"
	"testing"
)

type stubResponsesBackend struct{ models []Model }

func (s *stubResponsesBackend) ServeResponses(http.ResponseWriter, *http.Request, string) {}
func (s *stubResponsesBackend) Models(context.Context) ([]Model, error) {
	return append([]Model(nil), s.models...), nil
}

func TestProviderRouterUsesExplicitAntigravityPrefixOnly(t *testing.T) {
	codex := &stubResponsesBackend{}
	google := &stubResponsesBackend{}
	router := &ProviderRouter{Codex: codex, Antigravity: google}

	backend, model, err := router.Resolve("google-antigravity/gemini-3.8-flash")
	if err != nil || backend != google || model != "gemini-3.8-flash" {
		t.Fatalf("antigravity resolve backend=%T model=%q err=%v", backend, model, err)
	}
	backend, model, err = router.Resolve("gemini-3.8-flash")
	if err != nil || backend != codex || model != "gemini-3.8-flash" {
		t.Fatalf("bare model must stay on default Codex route: backend=%T model=%q err=%v", backend, model, err)
	}
	if _, _, err := router.Resolve("other-provider/model"); err == nil {
		t.Fatal("expected unknown provider prefix to fail explicitly")
	}
}

func TestProviderRouterMergesAndPrefixesModels(t *testing.T) {
	router := &ProviderRouter{
		Codex:       &stubResponsesBackend{models: []Model{{ID: "gpt-test", Object: "model", OwnedBy: "chatgpt-codex"}}},
		Antigravity: &stubResponsesBackend{models: []Model{{ID: "gemini-test"}}},
	}
	models, err := router.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Model{}
	for _, model := range models {
		got[model.ID] = model
	}
	if _, ok := got["gpt-test"]; !ok {
		t.Fatalf("models=%v", models)
	}
	google, ok := got["google-antigravity/gemini-test"]
	if !ok || google.OwnedBy != "google-antigravity" {
		t.Fatalf("models=%v", models)
	}
}
