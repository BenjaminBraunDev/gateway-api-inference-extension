package config

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"gopkg.in/yaml.v2"
)

func TestModelSubsetMapping_UnmarshalYAML(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    *ModelSubsetMapping
		wantErr bool
	}{
		{
			name: "valid full config",
			yaml: `
modelSubsets:
  - modelName: "model-a"
    podSelector:
      matchLabels:
        app: model-a-app
        env: prod
  - modelName: "model-b"
    podSelector:
      matchLabels:
        app: model-b-app
`,
			want: &ModelSubsetMapping{
				ModelSubsets: []ModelSubset{
					{
						ModelName: "model-a",
						PodSelector: &PodSelector{
							MatchLabels: map[string]string{"app": "model-a-app", "env": "prod"},
						},
					},
					{
						ModelName: "model-b",
						PodSelector: &PodSelector{
							MatchLabels: map[string]string{"app": "model-b-app"},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "empty modelSubsets list",
			yaml: `modelSubsets: []`,
			want: &ModelSubsetMapping{
				ModelSubsets: []ModelSubset{},
			},
			wantErr: false,
		},
		{
			name:    "empty yaml",
			yaml:    ``,
			want:    &ModelSubsetMapping{}, // Unmarshal into non-nil struct with nil/empty fields
			wantErr: false,
		},
		{
			name: "model with no selector",
			yaml: `
modelSubsets:
  - modelName: "model-c"
`,
			want: &ModelSubsetMapping{
				ModelSubsets: []ModelSubset{
					{ModelName: "model-c", PodSelector: nil},
				},
			},
			wantErr: false,
		},
		{
			name: "model with empty selector",
			yaml: `
modelSubsets:
  - modelName: "model-d"
    podSelector: {} # empty but valid selector
`,
			want: &ModelSubsetMapping{
				ModelSubsets: []ModelSubset{
					{ModelName: "model-d", PodSelector: &PodSelector{}},
				},
			},
			wantErr: false,
		},
		{
			name: "model with empty matchLabels",
			yaml: `
modelSubsets:
  - modelName: "model-e"
    podSelector:
      matchLabels: {}
`,
			want: &ModelSubsetMapping{
				ModelSubsets: []ModelSubset{
					{
						ModelName: "model-e",
						PodSelector: &PodSelector{
							MatchLabels: map[string]string{},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name:    "malformed yaml",
			yaml:    `modelSubsets: [ - modelName: "bad"`,
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got ModelSubsetMapping
			err := yaml.Unmarshal([]byte(tt.yaml), &got)
			if (err != nil) != tt.wantErr {
				t.Errorf("UnmarshalYAML() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if diff := cmp.Diff(tt.want, &got); diff != "" {
					t.Errorf("UnmarshalYAML() mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}
