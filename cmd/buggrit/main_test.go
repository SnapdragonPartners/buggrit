package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/config"
)

func TestConfigValidatePrintsEffectiveConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buggrit.json")
	doc := `{"version": 1, "models": {"default": {"provider": "ollama", "model": "qwen3-coder:30b"}}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"config", "validate", path}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
	// The printed config must round-trip as a valid config with the defaults filled in.
	cfg, err := config.Parse(out.Bytes())
	if err != nil {
		t.Fatalf("printed config does not parse: %v\n%s", err, out.String())
	}
	if cfg.Limits.PassDeadline != config.Defaults().Limits.PassDeadline {
		t.Errorf("pass_deadline = %v, want default", cfg.Limits.PassDeadline.Std())
	}
}

func TestConfigValidateReportsProblems(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buggrit.json")
	if err := os.WriteFile(path, []byte(`{"version": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"config", "validate", path}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "models.default: model is required") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"nope"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}
