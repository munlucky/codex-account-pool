package openaiapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type stubResponsesBackend struct{ models []Model }

func (s *stubResponsesBackend) ServeResponses(http.ResponseWriter, *http.Request, string) {}
func (s *stubResponsesBackend) Models(context.Context) ([]Model, error) {
	return append([]Model(nil), s.models...), nil
}

type brokenBackend struct{ stubResponsesBackend }

func (*brokenBackend) Models(context.Context) ([]Model, error) {
	return nil, errors.New("catalog unavailable")
}

func TestRegistrySupportsAnotherProviderWithoutRouterChanges(t *testing.T) {
	other := &stubResponsesBackend{models: []Model{{ID: "native"}}}
	r := &ProviderRouter{Default: "codex", Providers: map[string]ResponsesBackend{"codex": &brokenBackend{}, "another": other}}
	b, target, err := r.ResolveTarget("another/native", "")
	if err != nil || b != other || target.Provider != "another" || target.Model != "native" {
		t.Fatal(target, err)
	}
	models, err := r.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "another/native" {
		t.Fatal(models, err)
	}
	for _, model := range []string{"", "another/", "another/native/extra", "/native", "unknown/native"} {
		if _, _, err := r.ResolveTarget(model, ""); err == nil {
			t.Fatal("accepted", model)
		}
	}
	if _, _, err := r.ResolveTarget("another/native", "profile"); err == nil {
		t.Fatal("unsupported selector silently ignored")
	}
}

func TestProviderRouterUsesExplicitAntigravityPrefixOnly(t *testing.T) {
	codex := &stubResponsesBackend{}
	google := &stubResponsesBackend{}
	router := &ProviderRouter{Providers: map[string]ResponsesBackend{"codex": codex, "google-antigravity": google}}

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
		Providers: map[string]ResponsesBackend{
			"codex":              &stubResponsesBackend{models: []Model{{ID: "gpt-test", Object: "model", OwnedBy: "chatgpt-codex"}}},
			"google-antigravity": &stubResponsesBackend{models: []Model{{ID: "gemini-test"}}},
		},
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
