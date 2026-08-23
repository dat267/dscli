package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/alecthomas/kong"
)

// appName is the application name used for config path resolution. It is
// "dscli" for this binary and is overridden in generated projects via
// SetAppName.
var appName = "dscli"

// appDescription is shown in the top-level help.
var appDescription = "DeepSeek chat from your terminal"

const configFileFlagName = "config-file"

// SetAppName overrides the application name used for config path resolution.
func SetAppName(name string) {
	if name != "" {
		appName = name
	}
}

// SetAppDescription overrides the application description shown in help.
func SetAppDescription(desc string) {
	if desc != "" {
		appDescription = desc
	}
}

// App carries the resolved config file path to commands. Execute constructs
// one and binds it via Kong so config commands receive their path explicitly
// instead of reading a package global.
type App struct {
	cfgPath string
}

// CfgPath returns the config file path, resolving a default lazily when none
// is set. It is nil-safe.
func (a *App) CfgPath() string {
	if a == nil || a.cfgPath == "" {
		return resolveConfigPath()
	}
	return a.cfgPath
}

func resolveConfigPath() string {
	envKey := strings.ToUpper(appName) + "_CONFIG_FILE"
	if cf := os.Getenv(envKey); cf != "" {
		return cf
	}
	localFile := appName + ".json"
	if _, err := os.Stat(localFile); err == nil {
		return localFile
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, appName, appName+".json")
	}
	return localFile
}

// configOverrideWarning reports when the resolved config path silently comes
// from a file planted in the current directory. autoResolved is true when the
// path was selected by default resolution rather than an explicit
// --config-file flag, making the warning only fire on implicit use.
func configOverrideWarning(active string, autoResolved bool) string {
	if !autoResolved || active == "" {
		return ""
	}
	local := appName + ".json"
	if local != active {
		return ""
	}
	if _, err := os.Stat(local); err != nil {
		return ""
	}
	return fmt.Sprintf("warning: using config file %q from the current directory\n", active)
}

// Execute is the main entry point called by main.go.
func Execute(ctx context.Context) {
	app := &App{}
	explicit := resolveConfigFileFlag()
	if explicit != "" {
		app.cfgPath = explicit
	}
	activeConfig := app.CfgPath()
	if w := configOverrideWarning(activeConfig, explicit == ""); w != "" {
		fmt.Fprint(os.Stderr, w)
	}

	cli := &CLI{}
	options := []kong.Option{
		kong.Name(appName),
		kong.Description(appDescription),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true}),
		kong.BindTo(ctx, (*context.Context)(nil)),
		kong.Bind(app),
	}

	if resolver, warning := loadConfigResolver(activeConfig); warning != "" {
		fmt.Fprint(os.Stderr, warning)
	} else if resolver != nil {
		options = append(options, kong.Resolvers(resolver))
	}

	k, err := kong.New(cli, options...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	kongCtx, err := k.Parse(os.Args[1:])
	k.FatalIfErrorf(err)

	app.cfgPath = cli.ConfigFile
	k.FatalIfErrorf(kongCtx.Run())
}

func resolveConfigFileFlag() string {
	result := ""
	for i, arg := range os.Args {
		if arg == "--" {
			break
		}
		if arg == "--"+configFileFlagName && i+1 < len(os.Args) {
			result = os.Args[i+1]
		}
		if strings.HasPrefix(arg, "--"+configFileFlagName+"=") {
			result = strings.SplitN(arg, "=", 2)[1]
		}
	}
	return result
}

// loadConfigResolver opens and parses the config file at path. A non-empty
// warning is returned when the file exists but cannot be parsed, so an invalid
// config is surfaced instead of being silently ignored.
func loadConfigResolver(path string) (kong.Resolver, string) {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Sprintf("warning: ignoring config file %s: %v\n", path, err)
		}
		return nil, ""
	}
	defer func() { _ = f.Close() }()
	resolver, err := JSONResolver(f)
	if err != nil {
		return nil, fmt.Sprintf("warning: ignoring config file %s: %v\n", path, err)
	}
	return resolver, ""
}

// JSONResolver builds a Kong resolver capable of loading both flat and nested JSON configuration.
func JSONResolver(r io.Reader) (kong.Resolver, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	// Strip a UTF-8 byte order mark some editors add.
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	flat := make(map[string]any)

	var flattenNested func(prefix string, m map[string]any)
	flattenNested = func(prefix string, m map[string]any) {
		for k, v := range m {
			key := k
			if prefix != "" {
				key = prefix + "-" + k
			}
			if sub, ok := v.(map[string]any); ok {
				flattenNested(key, sub)
			} else if prefix != "" {
				flat[key] = v
			}
		}
	}
	flattenNested("", raw)

	for k, v := range raw {
		if _, isMap := v.(map[string]any); !isMap {
			flat[k] = v
		}
	}

	return kong.ResolverFunc(func(ctx *kong.Context, parent *kong.Path, flag *kong.Flag) (any, error) {
		val, ok := flat[flag.Name]
		if !ok {
			return nil, nil
		}
		// Coerce scalar JSON values to their string form for string-typed
		// flags, so a config like {"enabled": true} does not fail parsing when
		// the flag is a string (Kong's decoder would otherwise reject bool ->
		// string). Other flag types keep the native value.
		if flag.Target.IsValid() && flag.Target.Kind() == reflect.String {
			return fmt.Sprintf("%v", val), nil
		}
		return val, nil
	}), nil
}
