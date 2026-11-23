package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"gopkg.in/yaml.v3"
)

const (
	provider = "openai-responses"
	format   = "openai-response"
)

type config struct {
	BaseURL   string   `yaml:"base_url"`
	LegacyKey string   `yaml:"api_key"`
	APIKeyEnv string   `yaml:"api_key_env"`
	StateFile string   `yaml:"state_file"`
	Models    []string `yaml:"models"`

	legacyKey bool
	apiKey    string
	namespace [32]byte
}

type plugin struct {
	mu     sync.RWMutex
	config config
	db     *bolt.DB
	call   func(string, []byte) ([]byte, error)
	admin  credentialForm
}

var (
	modelID = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]*$`)
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func (p *plugin) configure(data []byte) error {
	cfg := config{
		BaseURL:   "https://api.openai.com/v1",
		APIKeyEnv: "OPENAI_RESPONSES_API_KEY",
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return errors.New("invalid provider configuration")
	}

	base, err := url.Parse(cfg.BaseURL)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil ||
		base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return errors.New("base_url must be an HTTPS API base URL without credentials, query, or fragment")
	}
	cfg.BaseURL = strings.TrimRight(base.String(), "/")
	if !envName.MatchString(cfg.APIKeyEnv) {
		return errors.New("api_key_env must name an environment variable")
	}
	legacyKey := strings.TrimSpace(cfg.LegacyKey)
	cfg.LegacyKey = ""
	cfg.legacyKey = legacyKey != ""
	envKey := strings.TrimSpace(os.Getenv(cfg.APIKeyEnv))
	if envKey != "" && !validKey(envKey) {
		return errors.New("invalid OpenAI project API key environment value")
	}
	if cfg.StateFile != "" && !filepath.IsAbs(cfg.StateFile) {
		return errors.New("state_file must be an absolute path")
	}
	if cfg.StateFile != "" {
		cfg.StateFile = filepath.Clean(cfg.StateFile)
	}
	if (envKey != "" || legacyKey != "") && (cfg.StateFile == "" || len(cfg.Models) == 0) {
		return errors.New("configure a state file and at least one upstream model before adding a key")
	}
	seen := make(map[string]bool, len(cfg.Models))
	for _, model := range cfg.Models {
		if !modelID.MatchString(model) || strings.HasSuffix(model, ":free") || seen[model] {
			return fmt.Errorf("invalid or duplicate upstream model %q", model)
		}
		seen[model] = true
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	db := p.db
	if cfg.StateFile == "" {
		db = nil
	} else if db == nil || p.config.StateFile != cfg.StateFile {
		var err error
		db, err = bolt.Open(cfg.StateFile, 0600, &bolt.Options{Timeout: time.Second})
		if err != nil {
			return fmt.Errorf("open provenance database: %w", err)
		}
	}
	if db != nil {
		key, stored, err := readCredential(db, cfg)
		if err != nil {
			if db != p.db {
				_ = db.Close()
			}
			return err
		}
		if !stored && envKey == "" && validKey(legacyKey) {
			if err := writeCredential(db, cfg, legacyKey); err != nil {
				if db != p.db {
					_ = db.Close()
				}
				return errors.New("could not encrypt the legacy project credential")
			}
			key = legacyKey
		}
		if key == "" {
			key = envKey
		}
		cfg.setKey(key)
	}
	if p.db != nil && p.db != db {
		_ = p.db.Close()
	}
	p.db = db
	p.config = cfg
	return nil
}

func (p *plugin) shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.db != nil {
		_ = p.db.Close()
		p.db = nil
	}
}

type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"http_status,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

func rejected(status int, code, message string) error {
	return &rpcError{Code: code, Message: message, Status: status}
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

func (p *plugin) dispatch(method string, data []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(data, &req); err != nil {
			return nil, err
		}
		if err := p.configure(req.ConfigYAML); err != nil {
			return nil, err
		}
		return map[string]any{
			"schema_version": 10,
			"metadata": map[string]any{
				"Name": provider, "Version": "0.2.0", "Author": "Tokligence",
				"GitHubRepository": "https://github.com/anschmieg/cpa-openai-responses",
				"ConfigFields": []map[string]string{
					{"Name": "base_url", "Type": "string", "Description": "HTTPS API base URL including the API prefix. Defaults to https://api.openai.com/v1."},
					{"Name": "models", "Type": "array", "Description": "Exact upstream model IDs to expose."},
					{"Name": "state_file", "Type": "string", "Description": "Absolute path to the persistent continuation database."},
				},
			},
			"capabilities": map[string]any{
				"model_registrar":         true,
				"management_api":          true,
				"model_router":            true,
				"executor":                true,
				"executor_model_scope":    "static",
				"executor_input_formats":  []string{format},
				"executor_output_formats": []string{format},
			},
		}, nil
	case "plugin.quiesce":
		// The host may resume this instance. Keep its state available until shutdown.
		return struct{}{}, nil
	case "plugin.shutdown":
		p.shutdown()
		return struct{}{}, nil
	case "management.register":
		return credentialRoutes(), nil
	case "management.handle":
		return p.handleCredential(data)
	case "executor.identifier":
		return map[string]string{"identifier": provider}, nil
	case "model.route":
		var req struct{ RequestedModel string }
		if err := json.Unmarshal(data, &req); err != nil {
			return nil, err
		}
		// Explicitly selected models use this executor's project key. They
		// must not enter the host's native OAuth credential selection.
		return map[string]any{
			"Handled": strings.HasPrefix(req.RequestedModel, provider+"/"), "TargetKind": "self",
		}, nil
	case "model.register":
		p.mu.RLock()
		defer p.mu.RUnlock()
		models := make([]map[string]any, 0, len(p.config.Models))
		if p.db == nil {
			return map[string]any{"Provider": provider, "Models": models}, nil
		}
		for _, model := range p.config.Models {
			id := provider + "/" + model
			models = append(models, map[string]any{
				"ID": id, "Name": id, "Object": "model", "OwnedBy": "openai",
				"UserDefined": true, "SupportedGenerationMethods": []string{"responses"},
			})
		}
		return map[string]any{"Provider": provider, "Models": models}, nil
	case "executor.execute", "executor.execute_stream":
		var req executorRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return nil, err
		}
		return p.execute(req, method == "executor.execute_stream")
	default:
		return nil, rejected(http.StatusNotImplemented, "unsupported_operation", "operation is unavailable for this provider")
	}
}

func (p *plugin) host(method string, req any, out any) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	raw, err = p.call(method, raw)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
