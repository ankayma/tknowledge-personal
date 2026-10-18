// Package config loads config.yaml, currently just the agents list
// (agents = the producer names that are machines).
package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Agents []string `yaml:"agents"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) AgentsSet() map[string]bool {
	set := make(map[string]bool, len(c.Agents))
	for _, a := range c.Agents {
		set[a] = true
	}
	return set
}
