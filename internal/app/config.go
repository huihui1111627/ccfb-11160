package app

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"bankcmp/internal/journal"

	"bankcmp/internal/model"
)

// ParseConfig parses a YAML task configuration.
func ParseConfig(text string) (*model.Config, error) {
	if strings.TrimSpace(text) == "" {
		return &model.Config{}, nil
	}
	var cfg model.Config
	if err := yaml.Unmarshal([]byte(text), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := normalizeConfig(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func normalizeConfig(cfg *model.Config) error {
	if cfg.Source.Driver != "" && cfg.Source.Driver != model.DriverSQLite && cfg.Source.Driver != model.DriverPostgres {
		return fmt.Errorf("source driver %q is not supported", cfg.Source.Driver)
	}
	if cfg.Target.Driver != "" && cfg.Target.Driver != model.DriverSQLite && cfg.Target.Driver != model.DriverPostgres {
		return fmt.Errorf("target driver %q is not supported", cfg.Target.Driver)
	}
	seen := map[string]bool{}
	for i := range cfg.Tables {
		t := &cfg.Tables[i]
		t.Name = strings.TrimSpace(t.Name)
		if t.Name == "" {
			return fmt.Errorf("table %d has no name", i)
		}
		if seen[strings.ToLower(t.Name)] {
			return fmt.Errorf("table %q is listed twice", t.Name)
		}
		seen[strings.ToLower(t.Name)] = true
		for j := range t.Columns {
			c := &t.Columns[j]
			if !c.Type.Valid() {
				return fmt.Errorf("table %q column %q: unknown type %q", t.Name, c.Name, c.Type)
			}
			if c.Type != model.TypeAuto && c.Type != model.TypeString && c.Type != model.TypeBytes {
				if c.SourceCharset != "" {
					return fmt.Errorf("table %q column %q: source_charset only applies to string/binary columns", t.Name, c.Name)
				}
			}
		}
	}
	return nil
}

// taskSpecs returns the table specifications to compare for a task.
func taskSpecs(t *journal.Task) ([]model.TableSpec, error) {
	cfg, err := ParseConfig(t.Config)
	if err != nil {
		return nil, err
	}
	var out []model.TableSpec
	for _, spec := range cfg.Tables {
		if spec.Ignore {
			continue
		}
		out = append(out, spec)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no tables configured for the task")
	}
	return out, nil
}
