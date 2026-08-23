package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ConfigCmdGroup struct {
	Init  ConfigInitCmd  `cmd:"" help:"Generate a default configuration file"`
	Path  ConfigPathCmd  `cmd:"" help:"Show configuration file path"`
	Show  ConfigShowCmd  `cmd:"" help:"Print current configuration values"`
	Set   ConfigSetCmd   `cmd:"" help:"Set a config value"`
	Unset ConfigUnsetCmd `cmd:"" help:"Unset a config value"`
}

type ConfigInitCmd struct {
	Overwrite bool `help:"Overwrite existing configuration file"`
}

func (cmd *ConfigInitCmd) Run(app *App) error {
	p := app.CfgPath()
	if _, err := os.Stat(p); err == nil && !cmd.Overwrite {
		return fmt.Errorf("configuration file already exists at %s", p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return fmt.Errorf("failed to create configuration directory: %w", err)
	}
	data, err := json.MarshalIndent(map[string]any{}, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal configuration: %w", err)
	}
	if err := os.WriteFile(p, data, 0600); err != nil {
		return fmt.Errorf("failed to write configuration file: %w", err)
	}
	fmt.Printf("Configuration file created at %s\n", p)
	return nil
}

type ConfigPathCmd struct{}

func (cmd *ConfigPathCmd) Run(app *App) error {
	p := app.CfgPath()
	if _, err := os.Stat(p); os.IsNotExist(err) {
		fmt.Printf("%s (does not exist)\n", p)
		return nil
	}
	fmt.Println(p)
	return nil
}

type ConfigShowCmd struct{}

func (cmd *ConfigShowCmd) Run(app *App) error {
	p := app.CfgPath()
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("%s (does not exist)\n", p)
			return nil
		}
		return fmt.Errorf("failed to read configuration file: %w", err)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	out := strings.TrimSuffix(string(data), "\n")
	out = strings.TrimSuffix(out, "\r")
	fmt.Println(out)
	return nil
}

type ConfigSetCmd struct {
	Key   string `arg:"" help:"Configuration key (dot-notation for nested, e.g. core.timeout)"`
	Value string `arg:"" help:"Value to set"`
}

func (cmd *ConfigSetCmd) Run(app *App) error {
	p := app.CfgPath()
	if err := validateConfigKey(cmd.Key); err != nil {
		return err
	}
	cfgMap, err := loadConfigMap(p)
	if err != nil {
		return err
	}

	var val any = cmd.Value
	if cmd.Value == "true" {
		val = true
	} else if cmd.Value == "false" {
		val = false
	} else if n, err := strconv.ParseFloat(cmd.Value, 64); err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) &&
		strconv.FormatFloat(n, 'f', -1, 64) == cmd.Value {
		val = n // Only lossless conversions (e.g. "5", "0.5"); strings like "00123" or "1e999" stay strings
	}

	keys := strings.Split(cmd.Key, ".")
	setNestedMap(cfgMap, keys, val)

	if err := saveConfigMap(p, cfgMap); err != nil {
		return err
	}
	fmt.Printf("Set %q = %v\n", cmd.Key, val)
	return nil
}

type ConfigUnsetCmd struct {
	Key string `arg:"" help:"Configuration key to unset"`
}

func (cmd *ConfigUnsetCmd) Run(app *App) error {
	p := app.CfgPath()
	if err := validateConfigKey(cmd.Key); err != nil {
		return err
	}
	cfgMap, err := loadConfigMap(p)
	if err != nil {
		return err
	}

	keys := strings.Split(cmd.Key, ".")
	if unsetNestedMap(cfgMap, keys) {
		if err := saveConfigMap(p, cfgMap); err != nil {
			return err
		}
		fmt.Printf("Unset %q\n", cmd.Key)
	} else {
		fmt.Printf("Key %q not found\n", cmd.Key)
	}
	return nil
}

// validateConfigKey rejects keys that are empty or contain empty dot-separated
// segments (e.g. ".foo", "foo.", "a..b"), which would create confusing
// empty-string map keys.
func validateConfigKey(key string) error {
	if key == "" {
		return fmt.Errorf("key cannot be empty")
	}
	for _, seg := range strings.Split(key, ".") {
		if seg == "" {
			return fmt.Errorf("key %q contains an empty segment", key)
		}
	}
	return nil
}

func loadConfigMap(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]any), nil
		}
		return nil, fmt.Errorf("failed to read configuration file: %w", err)
	}
	// Strip a UTF-8 byte order mark some editors add.
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("failed to parse configuration file: %w", err)
	}
	if m == nil {
		m = make(map[string]any)
	}
	return m, nil
}

func saveConfigMap(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}

func setNestedMap(m map[string]any, keys []string, val any) {
	if len(keys) == 0 {
		return
	}
	if len(keys) == 1 {
		m[keys[0]] = val
		return
	}
	sub, ok := m[keys[0]].(map[string]any)
	if !ok {
		sub = make(map[string]any)
		m[keys[0]] = sub
	}
	setNestedMap(sub, keys[1:], val)
}

func unsetNestedMap(m map[string]any, keys []string) bool {
	if len(keys) == 0 {
		return false
	}
	if len(keys) == 1 {
		if _, ok := m[keys[0]]; ok {
			delete(m, keys[0])
			return true
		}
		return false
	}
	sub, ok := m[keys[0]].(map[string]any)
	if !ok {
		return false
	}

	deleted := unsetNestedMap(sub, keys[1:])

	// Prune the parent map if it's now completely empty
	if deleted && len(sub) == 0 {
		delete(m, keys[0])
	}

	return deleted
}
