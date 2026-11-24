package main

import (
	"path/filepath"
	"testing"

	"github.com/tidwall/gjson"
	bolt "go.etcd.io/bbolt"
)

func TestPreviousResponseIDScope(t *testing.T) {
	database, err := bolt.Open(filepath.Join(t.TempDir(), "state.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	p := plugin{db: database, config: config{BaseURL: "https://example.invalid/v1", Models: []string{"test-model"}}}
	p.config.setKey("synthetic-project-key")
	original := p.config
	const previous = `,"previous_response_id":"resp_owned"`
	if err := p.recordResponseID(provenanceScope{original.namespace, "caller-one"}, gjson.Parse(`{"id":"resp_owned","object":"response"}`)); err != nil {
		t.Fatal(err)
	}
	check := func(caller, fields string, accepted bool) {
		t.Helper()
		_, _, err := p.prepare(executorRequest{
			Model:           "openai-responses/test-model",
			OriginalRequest: []byte(`{"model":"test-model"` + fields + `}`),
			Metadata:        map[string]any{"request_path": "/v1/responses", "caller_scope": caller},
		}, false)
		if (err == nil) != accepted {
			t.Fatalf("caller %q, fields %q: accepted=%t, err=%v", caller, fields, accepted, err)
		}
	}
	check("caller-one", previous, true)
	check("caller-two", previous, false)
	check("", previous, false)
	check("caller-one", `,"previous_response_id":null`+previous, false)
	p.config.setKey("rotated-project-key")
	check("caller-one", previous, false)
	check("caller-one", "", true)
	check("caller-one", `,"previous_response_id":null`, true)
	p.config = original
	p.config.BaseURL = "https://other.example.invalid/v1"
	p.config.setKey(original.apiKey)
	check("caller-one", previous, false)
	p.config = original
	check("caller-one", previous, true)
}
