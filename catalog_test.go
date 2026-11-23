package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestCatalogRegistrationRefresh(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("incorrect catalog request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, `{"data":[{"id":"gpt-5-mini"},{"id":"gpt-4.1"},{"id":"gpt-4o-realtime-preview"},{"id":"gpt-5-mini"},{"id":"text-embedding-3-large"}]}`)
		case 2:
			fmt.Fprint(w, `{"data":[{"id":"gpt-6.2-sol"}]}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	db, err := bolt.Open(filepath.Join(t.TempDir(), "state.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := plugin{db: db, config: config{BaseURL: server.URL + "/v1", apiKey: "test-key", namespace: [32]byte{1}}}
	check := func(want ...string) {
		t.Helper()
		value, err := p.dispatch("model.register", nil)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, row := range value.(map[string]any)["Models"].([]map[string]any) {
			got = append(got, row["ID"].(string))
		}
		if !slices.Equal(got, want) {
			t.Fatalf("registered %v; want %v", got, want)
		}
	}
	check("openai-responses/gpt-4.1", "openai-responses/gpt-5-mini")
	other := p.config
	other.namespace[0]++
	if p.catalog.contains(other, "gpt-5-mini") {
		t.Fatal("another credential inherited the catalog")
	}
	check("openai-responses/gpt-6.2-sol")
	if p.catalog.contains(p.config, "gpt-5-mini") || !p.catalog.contains(p.config, "gpt-6.2-sol") {
		t.Fatal("execution catalog did not reflect additions and removals")
	}
	if _, err := p.dispatch("model.register", nil); err == nil || p.catalog.contains(p.config, "gpt-6.2-sol") {
		t.Fatal("upstream error did not fail closed")
	}
	p.config.Models = []string{"custom-model"}
	check("openai-responses/custom-model")
	if calls.Load() != 3 {
		t.Fatal("pinned mode contacted the upstream catalog")
	}
}
