package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// knownDivergences is the reviewed list of cases the two implementations are
// allowed to disagree on.
//
// The shape is deliberately unforgiving. An entry needs an id, a reason a reader
// can evaluate, and an issue to close it -- because the failure mode of a list
// like this is that it becomes the place divergences go to be forgotten, and a
// bare id with no reason is indistinguishable from someone silencing a red run.
type knownDivergences struct {
	Divergences []knownDivergence `yaml:"divergences"`
}

type knownDivergence struct {
	ID     string `yaml:"id"`
	Reason string `yaml:"reason"`
	Issue  string `yaml:"issue"`
}

// loadKnownDivergences reads the file if one was given. No file means no accepted
// divergences, which is the correct default: the harness starts out demanding
// exact equivalence and each exception has to be argued for.
func loadKnownDivergences(path string) (map[string]bool, error) {
	if path == "" {
		return map[string]bool{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc knownDivergences
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	accepted := make(map[string]bool, len(doc.Divergences))
	for i, d := range doc.Divergences {
		switch {
		case d.ID == "":
			return nil, fmt.Errorf("%s: entry %d has no id", path, i+1)
		case d.Reason == "":
			return nil, fmt.Errorf("%s: %s has no reason", path, d.ID)
		case d.Issue == "":
			return nil, fmt.Errorf("%s: %s has no issue link", path, d.ID)
		case accepted[d.ID]:
			return nil, fmt.Errorf("%s: %s is listed twice", path, d.ID)
		}
		accepted[d.ID] = true
	}
	return accepted, nil
}
