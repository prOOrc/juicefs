/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"flag"
	"testing"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
)

// newMetaProxyOutboxTestContext builds a real cli.Context from the meta-proxy
// command flags, so c.IsSet reflects the parsed arguments (same pattern as the
// gateway tests).
func newMetaProxyOutboxTestContext(t *testing.T, args ...string) *cli.Context {
	t.Helper()
	set := flag.NewFlagSet("meta-proxy", flag.ContinueOnError)
	for _, f := range cmdMetaProxy().Flags {
		if err := f.Apply(set); err != nil {
			t.Fatalf("apply flag: %s", err)
		}
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse args %v: %s", args, err)
	}
	return cli.NewContext(nil, set, nil)
}

// TestOutboxConfFromContext (add-meta-proxy-outbox, tasks 2.1): the outbox
// flags wire into meta.Config.Outbox with mount parity — Enabled follows
// --outbox-enabled, StreamName is overridden only when --outbox-stream is
// explicitly set (its IsSet guard mirrors cmd/mount.go). Without flags the
// config equals the engine default: default-off is a zero behavior change.
func TestOutboxConfFromContext(t *testing.T) {
	testCases := []struct {
		name        string
		args        []string
		wantEnabled bool
		wantStream  string
	}{
		{
			name:        "flags unset: engine defaults, events off",
			args:        nil,
			wantEnabled: false,
			wantStream:  "juicefs:outbox",
		},
		{
			name:        "--outbox-enabled: events on, default stream",
			args:        []string{"--outbox-enabled"},
			wantEnabled: true,
			wantStream:  "juicefs:outbox",
		},
		{
			name:        "--outbox-enabled --outbox-stream: both applied",
			args:        []string{"--outbox-enabled", "--outbox-stream", "custom:stream"},
			wantEnabled: true,
			wantStream:  "custom:stream",
		},
		{
			// Mount parity: the IsSet guard overrides the stream name without
			// looking at --outbox-enabled; events stay off regardless.
			name:        "--outbox-stream alone: stream applies, events stay off",
			args:        []string{"--outbox-stream", "custom:stream"},
			wantEnabled: false,
			wantStream:  "custom:stream",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			conf := outboxConfFromContext(newMetaProxyOutboxTestContext(t, tc.args...))
			require.NotNil(t, conf)
			assert.Equal(t, tc.wantEnabled, conf.Outbox.Enabled)
			assert.Equal(t, tc.wantStream, conf.Outbox.StreamName)
		})
	}

	t.Run("default-off equals engine DefaultConf", func(t *testing.T) {
		conf := outboxConfFromContext(newMetaProxyOutboxTestContext(t))
		assert.Equal(t, meta.DefaultConf().Outbox, conf.Outbox)
	})

	t.Run("flag names defaults usages match mount surface", func(t *testing.T) {
		// The flags come from the shared registrar (outboxFlags); the
		// meta-proxy surface must stay identical to the mount one.
		proxy := flagSpecs(cmdMetaProxy().Flags)
		mount := flagSpecs(cmdMount().Flags)
		for _, name := range []string{"outbox-enabled", "outbox-stream"} {
			p, ok := proxy[name]
			require.True(t, ok, "meta-proxy must register --%s", name)
			m, ok := mount[name]
			require.True(t, ok, "mount must register --%s", name)
			assert.Equal(t, m, p, "meta-proxy flag --%s must equal the mount flag", name)
		}
	})
}

// outboxFlagSpec is the comparable surface of a flag: its rendered default
// text and usage string.
type outboxFlagSpec struct {
	defValue string
	usage    string
}

// flagSpecs extracts primary name → {default text, usage} for the flags in the
// set (the rendered default is the fork's equivalent of DefValue).
func flagSpecs(flags []cli.Flag) map[string]outboxFlagSpec {
	specs := make(map[string]outboxFlagSpec, len(flags))
	for _, f := range flags {
		df, ok := f.(cli.DocGenerationFlag)
		if !ok {
			continue
		}
		specs[df.Names()[0]] = outboxFlagSpec{df.GetDefaultText(), df.GetUsage()}
	}
	return specs
}
