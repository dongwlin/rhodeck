package output

import (
	"encoding/json"
	"testing"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/stretchr/testify/require"
)

// TestEnvelopeJSONIsStable pins the published machine contract: every
// envelope carries its own fields only, and the field names and order stay as
// published.
func TestEnvelopeJSONIsStable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "error",
			value: NewError(CodeInvalidPlan),
			want:  `{"schema_version":1,"type":"error","code":"invalid_plan"}`,
		},
		{
			name:  "help",
			value: Help{Header: NewHeader(KindHelp), Usage: "usage text"},
			want:  `{"schema_version":1,"type":"help","usage":"usage text"}`,
		},
		{
			name:  "version",
			value: Version{Header: NewHeader(KindVersion), Version: "dev"},
			want:  `{"schema_version":1,"type":"version","version":"dev"}`,
		},
		{
			name: "plan",
			value: Plan{
				Header:     NewHeader(KindPlan),
				Plan:       &control.Plan{SchemaVersion: 1},
				Tasks:      []control.TaskResult{},
				Sources:    &Sources{Input: "-", Worker: "/w", DataDir: "/d"},
				Unverified: []string{"unverified"},
			},
			want: `{"schema_version":1,"type":"plan","plan":{"schema_version":1,"initialize":{"snapshot_id":"","library_path":"",` +
				`"user_dir":"","resource_roots":null},"connection":{"adb_path":"","address":"","config":""},"tasks":null},` +
				`"tasks":[],"sources":{"input":"-","worker":"/w","data_dir":"/d"},"unverified":["unverified"]}`,
		},
		{
			name: "doctor",
			value: Doctor{
				Header:     NewHeader(KindDoctor),
				Code:       CodeEnvironmentRejected,
				Unverified: []string{"device connection"},
				Checks:     []Check{{Field: "worker", OK: true}},
			},
			want: `{"schema_version":1,"type":"doctor","code":"environment_rejected","unverified":["device connection"],` +
				`"checks":[{"field":"worker","ok":true}]}`,
		},
		{
			name: "terminal",
			value: Terminal{
				Header: NewHeader(KindTerminal),
				Result: &control.Result{SchemaVersion: 1, State: control.Succeeded, Cleanup: "complete"},
			},
			want: `{"schema_version":1,"type":"terminal","result":{"schema_version":1,"run_id":"","state":"succeeded",` +
				`"cleanup":"complete","started_at":"0001-01-01T00:00:00Z","finished_at":"0001-01-01T00:00:00Z","tasks":null}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.value)
			require.NoError(t, err)
			require.Equal(t, tc.want, string(raw), "machine envelope drifted")
		})
	}
}
