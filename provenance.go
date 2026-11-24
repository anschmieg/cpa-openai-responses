package main

import (
	"crypto/sha256"
	"strings"

	"github.com/tidwall/gjson"
	bolt "go.etcd.io/bbolt"
)

type provenanceScope struct {
	namespace [32]byte
	caller    string
}

var provenanceBucket = []byte("responses-opaque-v1")
var responseIDBucket = []byte("responses-ids-v1")

func (s provenanceScope) digest(value string) []byte {
	h := sha256.New()
	h.Write(s.namespace[:])
	h.Write([]byte(s.caller))
	h.Write([]byte{0})
	h.Write([]byte(value))
	return h.Sum(nil)
}

func validResponseID(value gjson.Result) bool {
	id := value.String()
	return value.Type == gjson.String && id != "" && len(id) <= 512 && strings.TrimSpace(id) == id
}

func (p *plugin) verifyResponseID(scope provenanceScope, previous gjson.Result) error {
	if previous.Type == gjson.Null {
		return nil
	}
	if !validResponseID(previous) {
		return rejected(400, "invalid_previous_response_id", "previous_response_id must be an upstream response ID")
	}
	unverified := rejected(400, "unverified_previous_response_id", "previous_response_id was not issued on this Responses route for this caller")
	if scope.caller == "" {
		return unverified
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.db == nil {
		return provenanceUnavailable()
	}
	found := false
	err := p.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(responseIDBucket)
		found = bucket != nil && bucket.Get(scope.digest(previous.String())) != nil
		return nil
	})
	if err != nil {
		return provenanceUnavailable()
	}
	if !found {
		return unverified
	}
	return nil
}

func (p *plugin) recordResponseID(scope provenanceScope, response gjson.Result) error {
	id := response.Get("id")
	if scope.caller == "" || response.Get("object").String() != "response" || !validResponseID(id) {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.db == nil {
		return provenanceUnavailable()
	}
	return p.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(responseIDBucket)
		if err != nil {
			return err
		}
		return bucket.Put(scope.digest(id.String()), []byte{1})
	})
}

func requestOpaque(root gjson.Result) ([]string, bool) {
	var tokens []string
	seen := make(map[string]bool)
	valid := true
	root.ForEach(func(k, value gjson.Result) bool {
		field := k.String()
		if seen[field] {
			valid = false
			return false
		}
		seen[field] = true
		switch field {
		case "input", "reasoning", "thinking":
			valid = inspectOpaque(value, &tokens)
		case "encrypted_content", "signature", "thought_signature", "thoughtSignature", "reasoning_opaque":
			valid = false
		}
		return valid
	})
	return tokens, valid
}

// Tokens for public OpenAI and Codex can use the same format. Only accept
// tokens previously observed from this upstream under the same caller scope.
func inspectOpaque(value gjson.Result, tokens *[]string) bool {
	valid := true
	if value.IsArray() || value.IsObject() {
		value.ForEach(func(key, field gjson.Result) bool {
			switch key.String() {
			case "signature", "thoughtSignature", "thought_signature", "reasoning_opaque":
				valid = field.Type == gjson.Null || (field.Type == gjson.String && field.String() == "")
			case "encrypted_content":
				valid = field.Type == gjson.Null || field.Type == gjson.String
				if token := field.String(); valid && token != "" {
					*tokens = append(*tokens, token)
				}
			default:
				valid = inspectOpaque(field, tokens)
			}
			return valid
		})
	}
	return valid
}

func (p *plugin) verify(scope provenanceScope, tokens []string) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.db == nil {
		return provenanceUnavailable()
	}
	if scope.caller == "" {
		return unknownOpaque()
	}
	return p.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(provenanceBucket)
		for _, token := range tokens {
			if bucket == nil || bucket.Get(scope.digest(token)) == nil {
				return unknownOpaque()
			}
		}
		return nil
	})
}

func (p *plugin) record(scope provenanceScope, output gjson.Result) error {
	if scope.caller == "" {
		return nil
	}
	var tokens []string
	items := []gjson.Result{output}
	if output.IsArray() {
		items = output.Array()
	}
	for _, item := range items {
		switch item.Get("type").String() {
		case "reasoning", "compaction":
			if token := item.Get("encrypted_content"); token.Type == gjson.String && token.String() != "" {
				tokens = append(tokens, token.String())
			}
		}
	}
	if len(tokens) == 0 {
		return nil
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.db == nil {
		return provenanceUnavailable()
	}
	return p.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(provenanceBucket)
		if err != nil {
			return err
		}
		for _, token := range tokens {
			key := scope.digest(token)
			if bucket.Get(key) == nil {
				if err := bucket.Put(key, []byte{1}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func unknownOpaque() error {
	return rejected(400, "unverified_reasoning_history", "opaque history did not originate on this Responses route for this caller; start a fresh conversation or continue it on its original route")
}

func provenanceUnavailable() error {
	return rejected(503, "provenance_unavailable", "Responses history verification is unavailable")
}
