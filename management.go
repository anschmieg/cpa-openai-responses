package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

const credentialRoute = "/plugins/openai-responses/credential"
const managementCredentialPath = "/v0/management" + credentialRoute
const resourceCredentialPath = "/v0/resource/plugins/openai-responses/credential"

type credentialForm struct {
	once sync.Once
	key  *rsa.PrivateKey
}

func (f *credentialForm) transportKey() *rsa.PrivateKey {
	f.once.Do(func() { f.key, _ = rsa.GenerateKey(rand.Reader, 3072) })
	return f.key
}

func credentialRoutes() any {
	return map[string]any{
		"routes": []map[string]string{
			{"Method": http.MethodGet, "Path": credentialRoute},
			{"Method": http.MethodPut, "Path": credentialRoute},
		},
		"resources": []map[string]string{
			{"Path": "/credential", "Menu": "OpenAI Responses Key"},
		},
	}
}

func credentialResponse(status int, body string, headers http.Header) any {
	headers.Set("Cache-Control", "no-store")
	headers.Set("X-Content-Type-Options", "nosniff")
	return map[string]any{"StatusCode": status, "Headers": headers, "Body": []byte(body)}
}

func (p *plugin) handleCredential(raw []byte) (any, error) {
	var req struct {
		Method, Path string
		Headers      http.Header
		Body         []byte
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.Method == http.MethodGet && req.Path == resourceCredentialPath {
		return credentialResponse(302, "", http.Header{"Location": {"/admin" + managementCredentialPath}}), nil
	}
	if req.Path != managementCredentialPath || req.Headers.Get("Sec-Fetch-Site") == "cross-site" {
		return credentialResponse(404, "", http.Header{}), nil
	}
	switch req.Method {
	case http.MethodGet:
		return p.credentialPage(), nil
	case http.MethodPut:
		if req.Headers.Get("Content-Type") != "application/json" || len(req.Body) > 1024 {
			return credentialResponse(400, "Invalid credential request.", http.Header{}), nil
		}
		var ciphertext []byte
		key := p.admin.transportKey()
		if key == nil || json.Unmarshal(req.Body, &ciphertext) != nil || len(ciphertext) != key.Size() {
			return credentialResponse(400, "Invalid credential request.", http.Header{}), nil
		}
		plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, ciphertext, nil)
		if err != nil || !validKey(string(plain)) {
			return credentialResponse(400, "Invalid credential. Reload the form and try again.", http.Header{}), nil
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.db == nil {
			return credentialResponse(409, "Save the state file in plugin settings first.", http.Header{}), nil
		}
		if err := writeCredential(p.db, p.config, string(plain)); err != nil {
			return credentialResponse(500, "Could not save the encrypted project key.", http.Header{}), nil
		}
		p.config.setKey(string(plain))
		return credentialResponse(204, "", http.Header{}), nil
	default:
		return credentialResponse(405, "", http.Header{}), nil
	}
}

func (p *plugin) credentialPage() any {
	key := p.admin.transportKey()
	if key == nil {
		return credentialResponse(500, "Credential form unavailable.", http.Header{})
	}
	publicKey, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return credentialResponse(500, "Credential form unavailable.", http.Header{})
	}
	nonce := base64.StdEncoding.EncodeToString(randBytes())
	if nonce == "" {
		return credentialResponse(500, "Credential form unavailable.", http.Header{})
	}
	p.mu.RLock()
	status := "No project key configured."
	if p.config.apiKey != "" {
		status = "A project key is configured."
	}
	if p.config.legacyKey {
		status += " Remove the old api_key setting from CPA configuration and rotate that key."
	}
	p.mu.RUnlock()
	return credentialResponse(200, fmt.Sprintf(credentialHTML, status, nonce, base64.StdEncoding.EncodeToString(publicKey)), http.Header{
		"Content-Type":            {"text/html; charset=utf-8"},
		"Content-Security-Policy": {"default-src 'none'; connect-src 'self'; frame-ancestors 'self'; script-src 'nonce-" + nonce + "'"},
		"Referrer-Policy":         {"no-referrer"},
	})
}

func randBytes() []byte {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return nil
	}
	return data
}

const credentialHTML = `<!doctype html><html lang="en"><meta charset="utf-8"><title>OpenAI Responses Key</title>
<h1>OpenAI Responses project key</h1><p role="status" id="status">%s</p>
<form id="key-form" autocomplete="off"><label for="project-key">Project API key</label>
<input id="project-key" type="password" autocomplete="new-password" maxlength="300" required>
<button type="submit">Save key</button></form>
<script nonce="%s">'use strict';
const form = document.getElementById('key-form');
const field = document.getElementById('project-key');
const status = document.getElementById('status');
form.addEventListener('submit', async (event) => {
  event.preventDefault();
  const button = form.querySelector('button');
  button.disabled = true;
  try {
    const key = await crypto.subtle.importKey('spki', Uint8Array.from(atob('%s'), c => c.charCodeAt(0)),
      {name: 'RSA-OAEP', hash: 'SHA-256'}, false, ['encrypt']);
    const ciphertext = await crypto.subtle.encrypt({name: 'RSA-OAEP'}, key, new TextEncoder().encode(field.value.trim()));
    field.value = '';
    const response = await fetch(location.pathname, {method: 'PUT', redirect: 'error',
      headers: {'Content-Type': 'application/json'}, body: JSON.stringify(btoa(String.fromCharCode(...new Uint8Array(ciphertext))))});
    status.textContent = response.ok ? 'Project key saved.' : await response.text();
  } catch {
    field.value = '';
    status.textContent = 'Could not save the project key. Reload the form and try again.';
  } finally {
    button.disabled = false;
  }
});</script></html>`
