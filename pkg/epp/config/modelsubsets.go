package config

// ModelSubsetMapping defines the overall structure of the model-subsets configuration file.
type ModelSubsetMapping struct {
	ModelSubsets []ModelSubset `yaml:"modelSubsets"`
}

// ModelSubset associates a model name with a specific pod selector.
type ModelSubset struct {
	ModelName   string       `yaml:"modelName"`
	PodSelector *PodSelector `yaml:"podSelector"`
}

// PodSelector defines criteria for selecting pods, currently by labels.
type PodSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}
