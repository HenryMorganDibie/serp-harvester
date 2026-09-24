package scheduler

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// harvestsFile is the shape of a YAML file listing scheduled harvests, e.g.:
//
//	harvests:
//	  - name: "morning-set-a"
//	    cron: "0 10 * * *"
//	    queries: ["best laptops 2026", "best phones 2026"]
//	    country: "US"
//	    language: "en"
type harvestsFile struct {
	Harvests []Harvest `yaml:"harvests"`
}

// LoadHarvests reads a YAML file of scheduled harvests. An empty or missing
// file (os.IsNotExist) returns an empty, non-error result — scheduling is
// entirely optional.
func LoadHarvests(path string) ([]Harvest, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scheduler: read %s: %w", path, err)
	}
	var f harvestsFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("scheduler: parse %s: %w", path, err)
	}
	return f.Harvests, nil
}
