package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupTestApp configures an App with a temporary config path.
func setupTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	return &App{cfgPath: filepath.Join(dir, "config.json")}
}

func TestConfigInitCmd_Run(t *testing.T) {
	app := setupTestApp(t)
	p := app.CfgPath()

	// 1. Test standard initialization
	initCmd := &ConfigInitCmd{}
	out := captureStdout(t, func() {
		if err := initCmd.Run(app); err != nil {
			t.Fatalf("unexpected error on init: %v", err)
		}
	})
	if !strings.Contains(out, "created at") {
		t.Errorf("expected success message, got: %s", out)
	}
	if _, err := os.Stat(p); os.IsNotExist(err) {
		t.Fatal("expected config file to be created on disk")
	}

	// 2. Test initialization without overwrite fails
	if err := initCmd.Run(app); err == nil {
		t.Fatal("expected error when initializing over existing file without Overwrite=true")
	}

	// 3. Test initialization with overwrite succeeds
	initCmd.Overwrite = true
	if err := initCmd.Run(app); err != nil {
		t.Fatalf("unexpected error with Overwrite=true: %v", err)
	}
}

func TestConfigPathCmd_Run(t *testing.T) {
	app := setupTestApp(t)
	p := app.CfgPath()

	// Test missing file
	out := captureStdout(t, func() {
		_ = (&ConfigPathCmd{}).Run(app)
	})
	if !strings.Contains(out, "(does not exist)") {
		t.Errorf("expected '(does not exist)', got: %s", out)
	}

	// Test existing file
	_ = (&ConfigInitCmd{}).Run(app)
	out = captureStdout(t, func() {
		_ = (&ConfigPathCmd{}).Run(app)
	})
	if !strings.Contains(out, p) {
		t.Errorf("expected path %s, got: %s", p, out)
	}
}

func TestConfigSetCmd_Types(t *testing.T) {
	app := setupTestApp(t)
	p := app.CfgPath()

	tests := []struct {
		key      string
		valIn    string
		expected any
	}{
		{"string_val", "hello world", "hello world"},
		{"bool_true", "true", true},
		{"bool_false", "false", false},
		{"int_val", "42", float64(42)}, // JSON unmarshals numbers to float64
	}

	for _, tc := range tests {
		cmd := &ConfigSetCmd{Key: tc.key, Value: tc.valIn}
		if err := cmd.Run(app); err != nil {
			t.Fatalf("failed to set %s: %v", tc.key, err)
		}
	}

	m, err := loadConfigMap(p)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	for _, tc := range tests {
		if val, ok := m[tc.key]; !ok {
			t.Errorf("key %s missing from config map", tc.key)
		} else if val != tc.expected {
			t.Errorf("key %s: expected %v (%T), got %v (%T)", tc.key, tc.expected, tc.expected, val, val)
		}
	}
}

func TestLoadConfigResolverReportsNonMissingOpenErrors(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	_, warning := loadConfigResolver(filepath.Join(parent, "config.json"))
	if warning == "" {
		t.Fatal("expected warning for non-missing config open error")
	}
}

func TestConfigSetCmd_LosslessCoercion(t *testing.T) {
	app := setupTestApp(t)
	p := app.CfgPath()

	tests := []struct {
		key      string
		valIn    string
		expected any
	}{
		{"s_leading_zero", "00123", "00123"},
		{"s_huge", "1e999", "1e999"},
		{"s_scientific", "1e3", "1e3"},
		{"s_float_style", "5.0", "5.0"},
		{"n_int", "42", float64(42)},
		{"n_float", "0.5", float64(0.5)},
	}

	for _, tc := range tests {
		if err := (&ConfigSetCmd{Key: tc.key, Value: tc.valIn}).Run(app); err != nil {
			t.Fatalf("failed to set %s: %v", tc.key, err)
		}
	}

	m, err := loadConfigMap(p)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	for _, tc := range tests {
		if got := m[tc.key]; got != tc.expected {
			t.Errorf("key %s: expected %v (%T), got %v (%T)", tc.key, tc.expected, tc.expected, got, got)
		}
	}
}

func TestConfigFilePermissions(t *testing.T) {
	app := setupTestApp(t)
	p := app.CfgPath()

	if err := (&ConfigInitCmd{}).Run(app); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0600 {
		t.Errorf("config init: expected 0600, got %v", got)
	}

	if err := (&ConfigSetCmd{Key: "a", Value: "b"}).Run(app); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	fi, err = os.Stat(p)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0600 {
		t.Errorf("config set: expected 0600, got %v", got)
	}
}

func TestConfigSetCmd_BOMPrefix(t *testing.T) {
	app := setupTestApp(t)
	p := app.CfgPath()
	bom := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"greeting":"hi"}`)...)
	if err := os.WriteFile(p, bom, 0600); err != nil {
		t.Fatal(err)
	}
	if err := (&ConfigSetCmd{Key: "extra", Value: "abc"}).Run(app); err != nil {
		t.Fatalf("config set on BOM-prefixed file failed: %v", err)
	}
	m, err := loadConfigMap(p)
	if err != nil {
		t.Fatalf("loadConfigMap: %v", err)
	}
	if m["greeting"] != "hi" || m["extra"] != "abc" {
		t.Errorf("expected BOM-prefixed config to be parseable, got %v", m)
	}
}

func TestConfigSetCmd_RejectsEmptySegments(t *testing.T) {
	app := setupTestApp(t)
	for _, key := range []string{".foo", "foo.", "a..b", ""} {
		if err := (&ConfigSetCmd{Key: key, Value: "x"}).Run(app); err == nil {
			t.Errorf("expected error for key %q, got nil", key)
		}
	}
	if err := (&ConfigUnsetCmd{Key: "a..b"}).Run(app); err == nil {
		t.Error("expected error for unset with empty segment, got nil")
	}
}

func TestConfigSetCmd_Nested(t *testing.T) {
	app := setupTestApp(t)
	p := app.CfgPath()

	// Set deep nested value
	_ = (&ConfigSetCmd{Key: "database.master.host", Value: "localhost"}).Run(app)
	_ = (&ConfigSetCmd{Key: "database.master.port", Value: "5432"}).Run(app)

	m, _ := loadConfigMap(p)
	db, ok := m["database"].(map[string]any)
	if !ok {
		t.Fatalf("expected 'database' to be map[string]any, got %T", m["database"])
	}
	master, ok := db["master"].(map[string]any)
	if !ok {
		t.Fatalf("expected 'database.master' to be map[string]any, got %T", db["master"])
	}
	if master["host"] != "localhost" {
		t.Errorf("expected host localhost, got %v", master["host"])
	}
	if master["port"] != float64(5432) {
		t.Errorf("expected port 5432, got %v", master["port"])
	}

	// Overwrite intermediate map with scalar
	_ = (&ConfigSetCmd{Key: "database.master", Value: "overwritten"}).Run(app)
	m, _ = loadConfigMap(p)
	db = m["database"].(map[string]any)
	if db["master"] != "overwritten" {
		t.Errorf("expected 'database.master' to be 'overwritten', got %v", db["master"])
	}

	// Overwrite scalar with map implicitly
	_ = (&ConfigSetCmd{Key: "database.master.new_key", Value: "1"}).Run(app)
	m, _ = loadConfigMap(p)
	db = m["database"].(map[string]any)
	master, ok = db["master"].(map[string]any)
	if !ok {
		t.Fatalf("expected 'database.master' to revert to map, got %T", db["master"])
	}
	if master["new_key"] != float64(1) {
		t.Errorf("expected new_key to be 1")
	}
}

func TestConfigUnsetCmd(t *testing.T) {
	app := setupTestApp(t)
	p := app.CfgPath()

	// Setup initial config
	_ = (&ConfigSetCmd{Key: "app.name", Value: "min"}).Run(app)
	_ = (&ConfigSetCmd{Key: "app.debug", Value: "true"}).Run(app)
	_ = (&ConfigSetCmd{Key: "version", Value: "v1.0"}).Run(app)

	// Unset nested key
	out := captureStdout(t, func() {
		_ = (&ConfigUnsetCmd{Key: "app.debug"}).Run(app)
	})
	if !strings.Contains(out, "Unset \"app.debug\"") {
		t.Errorf("expected success message, got %s", out)
	}

	m, _ := loadConfigMap(p)
	appMap := m["app"].(map[string]any)
	if _, exists := appMap["debug"]; exists {
		t.Error("expected app.debug to be deleted")
	}
	if appMap["name"] != "min" {
		t.Error("expected app.name to remain intact")
	}

	// Unset missing key
	out = captureStdout(t, func() {
		_ = (&ConfigUnsetCmd{Key: "app.missing"}).Run(app)
	})
	if !strings.Contains(out, "not found") {
		t.Errorf("expected not found message, got %s", out)
	}

	// Unset top-level key containing nested maps
	_ = (&ConfigUnsetCmd{Key: "app"}).Run(app)
	m, _ = loadConfigMap(p)
	if _, exists := m["app"]; exists {
		t.Error("expected top level 'app' to be deleted entirely")
	}
	if m["version"] != "v1.0" {
		t.Error("expected 'version' to remain intact")
	}
}

func TestSaveConfigMap_Formatting(t *testing.T) {
	app := setupTestApp(t)
	p := app.CfgPath()
	m := map[string]any{"hello": "world", "nested": map[string]any{"count": 3}}
	if err := saveConfigMap(p, m); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// Verify file ends with newline (POSIX standard)
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Error("expected config file to end with a newline character")
	}

	// Round-trip: the written file must parse back to the original map.
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("written config does not parse as JSON: %v\n%s", err, data)
	}
	if parsed["hello"] != "world" {
		t.Errorf("expected hello=world after round-trip, got %v", parsed["hello"])
	}
	if nested, ok := parsed["nested"].(map[string]any); !ok {
		t.Errorf("expected nested to be a map, got %T", parsed["nested"])
	} else if nested["count"] != float64(3) {
		t.Errorf("expected nested.count=3 after round-trip, got %v", nested["count"])
	}
}

func TestApp_IndependentPaths(t *testing.T) {
	appA := &App{cfgPath: filepath.Join(t.TempDir(), "a.json")}
	appB := &App{cfgPath: filepath.Join(t.TempDir(), "b.json")}

	if err := (&ConfigSetCmd{Key: "greeting", Value: "hi"}).Run(appA); err != nil {
		t.Fatalf("set on A: %v", err)
	}
	if err := (&ConfigSetCmd{Key: "greeting", Value: "yo"}).Run(appB); err != nil {
		t.Fatalf("set on B: %v", err)
	}

	ma, err := loadConfigMap(appA.CfgPath())
	if err != nil {
		t.Fatal(err)
	}
	mb, err := loadConfigMap(appB.CfgPath())
	if err != nil {
		t.Fatal(err)
	}
	if ma["greeting"] != "hi" {
		t.Errorf("A.greeting = %v, want hi", ma["greeting"])
	}
	if mb["greeting"] != "yo" {
		t.Errorf("B.greeting = %v, want yo", mb["greeting"])
	}
}

func TestApp_CfgPathLazyFallback(t *testing.T) {
	// An App with no explicit path falls back to the default resolution.
	app := &App{}
	if got := app.CfgPath(); got == "" {
		t.Error("expected a resolved default path, got empty")
	}
}
