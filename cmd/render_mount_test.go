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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"

	"github.com/juicedata/juicefs/pkg/utils"
)

func parseRenderFlags(t *testing.T, args ...string) *cli.Context {
	t.Helper()
	var ctx *cli.Context
	app := &cli.App{
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "attr-cache", Value: "1.0s"},
			&cli.StringFlag{Name: "entry-cache", Value: "1.0s"},
			&cli.StringFlag{Name: "dir-entry-cache", Value: "1.0s"},
		},
		Action: func(c *cli.Context) error {
			ctx = c
			return nil
		},
	}
	require.NoError(t, app.Run(append([]string{"juicefs"}, args...)))
	return ctx
}

// TestForceRenderCacheTimeouts_DefaultsRaised (task 4.3): without explicit
// operator values the kernel-side cache timeouts are raised to at least 60s.
func TestForceRenderCacheTimeouts_DefaultsRaised(t *testing.T) {
	c := parseRenderFlags(t)
	forceRenderCacheTimeouts(c)
	for _, name := range []string{"attr-cache", "entry-cache", "dir-entry-cache"} {
		require.GreaterOrEqual(t, utils.Duration(c.String(name)), renderCacheTimeout, name)
	}
}

// TestForceRenderCacheTimeouts_ExplicitRespected: an explicit operator value is
// kept as-is (even below 60s); the other flags are still raised.
func TestForceRenderCacheTimeouts_ExplicitRespected(t *testing.T) {
	c := parseRenderFlags(t, "--attr-cache", "5s")
	forceRenderCacheTimeouts(c)
	require.Equal(t, 5*time.Second, utils.Duration(c.String("attr-cache")))
	require.GreaterOrEqual(t, utils.Duration(c.String("entry-cache")), renderCacheTimeout)
	require.GreaterOrEqual(t, utils.Duration(c.String("dir-entry-cache")), renderCacheTimeout)
}
