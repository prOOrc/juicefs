//go:build windows
// +build windows

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

package winfsp

import (
	"flag"
	"strings"
	"testing"

	"github.com/urfave/cli/v2"
)

// newTestContext builds a *cli.Context as the real app would: flags are
// applied to a flag.FlagSet, the args are parsed, and ctx.Command is set so
// that buildServiceCmdLine can iterate the command's flags.
func newTestContext(t *testing.T, flags []cli.Flag, args []string, app *cli.App, parent *cli.Context) *cli.Context {
	t.Helper()
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	for _, f := range flags {
		if err := f.Apply(set); err != nil {
			t.Fatalf("apply flag %v: %v", f.Names(), err)
		}
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse args %v: %v", args, err)
	}
	ctx := cli.NewContext(app, set, parent)
	ctx.Command = &cli.Command{Name: "mount", Flags: flags}
	return ctx
}

// mountFlags returns the flags exercised by the tests. winfsp cannot import
// cmd, so the declarations mirror the relevant subset of cmd/mount_windows.go.
func mountFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{
			Name:  "sts-enabled",
			Usage: "use short-lived STS storage credentials via the meta proxy (encrypted volumes only, requires a grpc:// meta endpoint)",
		},
		&cli.StringFlag{
			Name:  "company-id",
			Usage: "company ID for STS credential requests (required with --sts-enabled)",
		},
		&cli.StringFlag{
			Name:  "cache-dir",
			Usage: "directory paths of local cache files",
		},
		&cli.BoolFlag{
			Name:    "d",
			Aliases: []string{"background"},
			Usage:   "run in background(Windows: as a system service. support ONLY 1 volume mounting at the same time)",
		},
		&cli.StringFlag{
			Name:  "alias",
			Usage: "volume alias, useful for mounting a volume multiple times on the same machine",
		},
		&cli.StringFlag{
			Name:  "delay-close",
			Usage: "delay file closing duration",
		},
	}
}

func indexOf(cmds []string, want string) int {
	for i, c := range cmds {
		if c == want {
			return i
		}
	}
	return -1
}

const testEncodedURL = `grpc://proxy.example.com:9561/myvol?oidc-issuer=https%3A%2F%2Fidp.example.com%2Frealms%2Fagio&oidc-client-id=agio-drive&oidc-redirect-url=http%3A%2F%2Flocalhost%3A8090%2Fcallback&oidc-scopes=openid+profile&tls=1&tls-ca=C%3A%5Ccerts%5Cca.pem&cache-size=2048`

func TestBuildServiceCmdLine(t *testing.T) {
	ctx := newTestContext(t, mountFlags(),
		[]string{"-d", "--sts-enabled", "--company-id", "company-42", testEncodedURL, "Z:"},
		&cli.App{}, nil)
	cmds := buildServiceCmdLine(ctx, ctx.Args().Get(0), `C:\ProgramData\juicefs\cache`)

	if cmds[0] != "mount" {
		t.Errorf("cmds[0] = %q, want %q", cmds[0], "mount")
	}
	if cmds[1] != testEncodedURL {
		t.Errorf("meta URL not preserved byte-for-byte:\ngot  %q\nwant %q", cmds[1], testEncodedURL)
	}
	if cmds[2] != "%2" {
		t.Errorf("cmds[2] = %q, want %q", cmds[2], "%2")
	}

	if indexOf(cmds, "--sts-enabled=true") < 0 {
		t.Errorf("cmds %v does not contain %q", cmds, "--sts-enabled=true")
	}

	i := indexOf(cmds, "--company-id")
	if i < 0 {
		t.Fatalf("cmds %v does not contain %q", cmds, "--company-id")
	}
	if i+1 >= len(cmds) || cmds[i+1] != "\"company-42\"" {
		t.Errorf("cmds %v: %q is not followed by %q", cmds, "--company-id", "\"company-42\"")
	}

	for _, absent := range []string{"-d=true", "--background=true"} {
		if indexOf(cmds, absent) >= 0 {
			t.Errorf("cmds %v unexpectedly contains %q", cmds, absent)
		}
	}

	tail := []string{"--alias", "\"%1\"", "--cache-dir", "\"C:\\ProgramData\\juicefs\\cache\""}
	if len(cmds) < len(tail) {
		t.Fatalf("cmds %v is shorter than the expected tail %v", cmds, tail)
	}
	gotTail := cmds[len(cmds)-len(tail):]
	if strings.Join(gotTail, " ") != strings.Join(tail, " ") {
		t.Errorf("cmds tail = %v, want %v", gotTail, tail)
	}
}

func TestBuildServiceCmdLineExplicitCacheDir(t *testing.T) {
	ctx := newTestContext(t, mountFlags(),
		[]string{"-d", "--sts-enabled", "--company-id", "company-42", "--cache-dir", `D:\jfs-cache`, testEncodedURL, "Z:"},
		&cli.App{}, nil)
	cmds := buildServiceCmdLine(ctx, ctx.Args().Get(0), `C:\ProgramData\juicefs\cache`)

	i := indexOf(cmds, "--cache-dir")
	if i < 0 {
		t.Fatalf("cmds %v does not contain %q", cmds, "--cache-dir")
	}
	if i+1 >= len(cmds) || cmds[i+1] != "\"D:\\jfs-cache\"" {
		t.Errorf("cmds %v: %q is not followed by %q", cmds, "--cache-dir", "\"D:\\jfs-cache\"")
	}
	if indexOf(cmds[i+1:], "--cache-dir") >= 0 {
		t.Errorf("cmds %v contains %q more than once", cmds, "--cache-dir")
	}
	if indexOf(cmds, "\"C:\\ProgramData\\juicefs\\cache\"") >= 0 {
		t.Errorf("cmds %v unexpectedly contains the default cache dir", cmds)
	}
}

func TestBuildServiceCmdLineRawURL(t *testing.T) {
	rawURL := `grpc://host:9561/vol?oidc-issuer=https://idp&oidc-client-id=drive`
	ctx := newTestContext(t, mountFlags(),
		[]string{rawURL, "Z:"},
		&cli.App{}, nil)
	cmds := buildServiceCmdLine(ctx, ctx.Args().Get(0), "")

	if cmds[1] != rawURL {
		t.Errorf("raw meta URL not preserved byte-for-byte:\ngot  %q\nwant %q", cmds[1], rawURL)
	}
}

func TestBuildServiceCmdLineAppFlag(t *testing.T) {
	app := &cli.App{Flags: []cli.Flag{&cli.BoolFlag{Name: "debug"}}}
	appSet := flag.NewFlagSet("app", flag.ContinueOnError)
	if err := app.Flags[0].Apply(appSet); err != nil {
		t.Fatalf("apply app flag: %v", err)
	}
	if err := appSet.Parse([]string{"--debug=true"}); err != nil {
		t.Fatalf("parse app args: %v", err)
	}
	parent := cli.NewContext(app, appSet, nil)

	ctx := newTestContext(t, mountFlags(), []string{testEncodedURL, "Z:"}, app, parent)
	cmds := buildServiceCmdLine(ctx, ctx.Args().Get(0), "")

	if indexOf(cmds, "--debug=true") < 0 {
		t.Errorf("cmds %v does not contain the app flag %q", cmds, "--debug=true")
	}
}
