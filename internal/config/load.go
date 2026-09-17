package config

import (
	"fmt"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

const defaultEnvPrefix = "NCGO_"

// Load merges Default < file (if Path is set) < environment < Overrides,
// then validates the result. EnvPrefix defaults to "NCGO_". Nested keys use
// a double underscore: NCGO_DATABASE__DSN → database.dsn. Compatibility
// aliases: NCGO_SECRET → instance.secret, NCGO_INSTANCE_ID → instance.id,
// NCGO_MAINTENANCE → maintenance.enabled.
func Load(opts LoadOptions) (*Config, error) {
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(defaultFlat(), "."), nil); err != nil {
		return nil, fmt.Errorf("config: load defaults: %w", err)
	}

	if opts.Path != "" {
		if err := k.Load(file.Provider(opts.Path), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("config: load file %s: %w", opts.Path, err)
		}
	}

	prefix := opts.EnvPrefix
	if prefix == "" {
		prefix = defaultEnvPrefix
	}
	if err := k.Load(env.Provider(".", env.Opt{
		Prefix: prefix,
		TransformFunc: func(key, val string) (string, any) {
			return transformEnv(prefix, key, val)
		},
	}), nil); err != nil {
		return nil, fmt.Errorf("config: load env: %w", err)
	}

	if len(opts.Overrides) > 0 {
		if err := k.Load(confmap.Provider(opts.Overrides, "."), nil); err != nil {
			return nil, fmt.Errorf("config: load overrides: %w", err)
		}
	}

	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func transformEnv(prefix, key, val string) (string, any) {
	switch key {
	case prefix + "SECRET":
		return "instance.secret", val
	case prefix + "INSTANCE_ID":
		return "instance.id", val
	case prefix + "MAINTENANCE":
		return "maintenance.enabled", envBool(val)
	}
	trimmed := strings.TrimPrefix(key, prefix)
	trimmed = strings.ToLower(trimmed)
	return strings.Join(strings.Split(trimmed, "__"), "."), val
}

func envBool(v string) any {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off", "":
		return false
	default:
		return v
	}
}
