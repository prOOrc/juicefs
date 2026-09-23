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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/urfave/cli/v2"
	"google.golang.org/grpc/metadata"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/juicedata/juicefs/pkg/version"
	"github.com/juicedata/juicefs/pkg/vfs"
)

// renderKEKSize must match meta.kekSize (AES-256 Company KEK).
const renderKEKSize = 32

// renderCacheTimeout is the minimum kernel-side attr/entry cache timeout for
// render mounts: render workloads re-stat the same files thousands of times, so
// the kernel cache must absorb them instead of hitting Redis (task 4.3).
const renderCacheTimeout = 60 * time.Second

func cmdRenderMount() *cli.Command {
	return &cli.Command{
		Name:      "render-mount",
		Action:    renderMount,
		Category:  "SERVICE",
		Usage:     "Mount a volume for a render node (direct Redis + S3, Company KEK)",
		ArgsUsage: "META-URL MOUNTPOINT",
		Description: `
Mount the target volume at the mount point for a render node (SRS §7): direct
metadata backend access with local FEK handling under the Company KEK. No Meta
Proxy, no OIDC, no per-file KeyManager RPCs — the KEK is fetched once at mount
from the platform's DriveKeyManagerService using the node's YC IAM identity and
is the node's only trust anchor (FR-RND-2). The mount is chrooted to
companies/{--company-code} as defense in depth on top of the cryptographic
isolation (decision 4.2).

The command runs in the foreground; a supervisor (e.g. Kubernetes) should
restart it. The Company KEK is never accepted via CLI or environment — only
over the TLS 1.3 KeyManager channel (NFR-SEC-2).

Examples:
# Mount company "acme" data on a render node (IAM token from instance metadata)
$ juicefs render-mount redis://localhost /mnt/render \
    --company-id 6f1e... --company-code acme \
    --keymanager-service keymanager.agio.svc:9000 --keymanager-tls-ca /etc/ssl/agio/ca.pem`,
		Flags: expandFlags(renderMountFlags(), clientFlags(1.0), shareInfoFlags()),
	}
}

func renderMountFlags() []cli.Flag {
	return addCategories("RENDER", []cli.Flag{
		&cli.StringFlag{
			Name:  "company-id",
			Usage: "company ID (UUID) of this node's company; used for FetchCompanyKEK and bound into the FEK AAD",
		},
		&cli.StringFlag{
			Name:  "company-code",
			Usage: "company code; the mount is chrooted to companies/{code}",
		},
		&cli.StringFlag{
			Name:  "keymanager-service",
			Usage: "gRPC address (host:port) of the platform DriveKeyManagerService",
		},
		&cli.StringFlag{
			Name:  "keymanager-tls-ca",
			Usage: "CA certificate (PEM file) for the KeyManager channel; TLS 1.3 is enforced when set, insecure transport otherwise (dev only)",
		},
		&cli.StringFlag{
			Name:  "keymanager-server-name",
			Usage: "TLS server name override for the KeyManager channel",
		},
		&cli.StringFlag{
			Name:  "iam-token-file",
			Usage: "file containing the YC IAM token; when empty the token is fetched from the instance metadata service (169.254.169.254)",
		},
		&cli.StringFlag{
			Name:  "node-id",
			Usage: "node identifier reported to FetchCompanyKEK for audit; defaults to the hostname",
		},
		&cli.BoolFlag{
			Name:  "sts-enabled",
			Usage: "use short-lived STS storage credentials (AWS: AssumeRole with the node instance profile; YC: ephemeral access keys from the node IAM token) instead of static keys in the volume format",
		},
		&cli.StringFlag{
			Name:  "sts-role-arn",
			Usage: "IAM role ARN to assume for storage access (required with --sts-enabled on AWS volumes)",
		},
	})
}

// iamTokenProvider yields the YC IAM token that authenticates the render node to
// FetchCompanyKEK (the platform verifies it via YC IAM introspection).
type iamTokenProvider interface {
	Token(ctx context.Context) (string, error)
}

// ycMetadataTokenProvider fetches a service-account identity token from the YC
// instance metadata service — the production path on render nodes.
type ycMetadataTokenProvider struct {
	audience string
}

func (p *ycMetadataTokenProvider) Token(ctx context.Context) (string, error) {
	endpoint := "http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/identity?audience=" + url.QueryEscape(p.audience)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch IAM token from instance metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("instance metadata returned %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", errors.New("empty IAM token from instance metadata")
	}
	return token, nil
}

// fileTokenProvider reads the token from a file (local development and tests).
type fileTokenProvider struct {
	path string
}

func (p *fileTokenProvider) Token(ctx context.Context) (string, error) {
	body, err := os.ReadFile(p.path)
	if err != nil {
		return "", fmt.Errorf("read IAM token file: %w", err)
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", errors.New("empty IAM token file")
	}
	return token, nil
}

// fetchCompanyKEK retrieves the Company KEK for companyID, authenticating with
// the node's IAM token in the "authorization" gRPC metadata (Bearer scheme).
func fetchCompanyKEK(ctx context.Context, km meta.KeyManagerClient, tp iamTokenProvider, companyID, nodeID string) ([]byte, uint32, error) {
	token, err := tp.Token(ctx)
	if err != nil {
		return nil, 0, err
	}
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	resp, err := km.FetchCompanyKEK(ctx, &kmpb.FetchCompanyKEKRequest{CompanyId: companyID, NodeId: nodeID})
	if err != nil {
		return nil, 0, fmt.Errorf("FetchCompanyKEK: %w", err)
	}
	if len(resp.Kek) != renderKEKSize {
		return nil, 0, fmt.Errorf("unexpected Company KEK size %d (want %d)", len(resp.Kek), renderKEKSize)
	}
	return resp.Kek, resp.KekVersion, nil
}

// forceRenderCacheTimeouts enforces aggressive kernel-side caching for render
// mounts (task 4.3): attr/entry/dir-entry timeouts of at least 60s. Explicit
// operator values are respected. Readdir needs no pipeline change:
// redisMeta.doReaddir already batches the directory read (HSCAN) and the attr
// fill (MGet) into one round-trip each.
func forceRenderCacheTimeouts(c *cli.Context) {
	force := func(name string) {
		if c.IsSet(name) {
			return
		}
		if d := utils.Duration(c.String(name)); d < renderCacheTimeout {
			_ = c.Set(name, renderCacheTimeout.String())
			logger.Infof("render-mount: %s raised to %s for aggressive caching", name, renderCacheTimeout)
		}
	}
	force("attr-cache")
	force("entry-cache")
	force("dir-entry-cache")
}

func renderMount(c *cli.Context) error {
	setup(c, 2)
	addr := c.Args().Get(0)
	removePassword(addr)
	mp := c.Args().Get(1)

	companyID := c.String("company-id")
	companyCode := c.String("company-code")
	kmsAddr := c.String("keymanager-service")
	if companyID == "" || companyCode == "" || kmsAddr == "" {
		logger.Fatalf("render-mount requires --company-id, --company-code and --keymanager-service")
	}

	mp, err := filepath.Abs(mp)
	if err != nil {
		logger.Fatalf("abs %q: %s", mp, err)
	}
	if mp == "/" {
		logger.Fatalf("should not mount on the root directory")
	}
	prepareMp(mp)

	// 1. Fetch the Company KEK — the node's only trust anchor (FR-RND-2). It is
	//    never passed via CLI: the node authenticates with its IAM identity and
	//    receives the KEK over TLS 1.3 (NFR-SEC-2).
	var tp iamTokenProvider
	if f := c.String("iam-token-file"); f != "" {
		tp = &fileTokenProvider{path: f}
	} else {
		tp = &ycMetadataTokenProvider{audience: kmsAddr}
	}
	nodeID := c.String("node-id")
	if nodeID == "" {
		nodeID, _ = os.Hostname()
	}
	km, err := meta.NewRenderKeyManagerClient(kmsAddr, c.String("keymanager-tls-ca"), c.String("keymanager-server-name"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	kek, kekVersion, err := fetchCompanyKEK(ctx, km, tp, companyID, nodeID)
	cancel()
	if err != nil {
		_ = km.Close()
		return fmt.Errorf("fetch company KEK: %s", err)
	}
	if cerr := km.Close(); cerr != nil {
		logger.Warnf("close keymanager client: %s", cerr)
	}
	mlockKey(kek)
	logger.Infof("Company KEK fetched (version %d), node %s, company %s", kekVersion, nodeID, companyID)

	// 2. Direct backend access — no proxy, no OIDC, no authz (SRS §7).
	metaConf := getMetaConf(c, mp, c.Bool("read-only") || utils.StringContains(strings.Split(c.String("o"), ","), "ro"))
	prefix := "companies/" + companyCode
	if metaConf.Subdir != "" {
		logger.Warnf("--subdir %q is ignored by render-mount; using %q", metaConf.Subdir, prefix)
	}
	metaConf.Subdir = prefix

	metaCli := meta.NewClient(addr, metaConf)
	format, err := metaCli.Load(true)
	if err != nil {
		return err
	}
	if !format.EncryptionEnabled {
		logger.Fatalf("volume %q has encryption disabled; render-mount requires an encrypted volume", format.Name)
	}

	chunkConf := getChunkConf(c, format)
	vfsConf := getVfsConf(c, metaConf, format, chunkConf)
	setFuseOption(c, format, vfsConf)
	forceRenderCacheTimeouts(c)

	blob, err := NewReloadableStorage(format, metaCli, updateFormat(c))
	if err != nil {
		return fmt.Errorf("object storage: %s", err)
	}
	logger.Infof("Data use %s", blob)

	// STS credentials (task 7.4): replace the static storage credentials with
	// short-lived ones scoped to the company prefix. AWS: AssumeRole with the
	// node's instance profile; YC: ephemeral access keys issued from the node's
	// IAM token (no static key involved). Fail-fast on the initial fetch.
	if c.Bool("sts-enabled") {
		holder, ok := blob.(*storageHolder)
		if !ok {
			return fmt.Errorf("sts: storage is not reloadable")
		}
		var provider stsCredentialsProvider
		switch format.Storage {
		case "aws":
			roleARN := c.String("sts-role-arn")
			if roleARN == "" {
				return fmt.Errorf("--sts-role-arn is required with --sts-enabled on AWS volumes")
			}
			provider = &awsAssumeRoleProvider{
				roleARN:     roleARN,
				sessionName: nodeID,
				bucket:      format.Bucket,
				prefix:      prefix,
				duration:    defaultSTSTTL,
			}
		default:
			provider = &ycEphemeralKeyProvider{
				token:       tp.Token,
				sessionName: nodeID,
				policy:      ycPrefixPolicy(format.Bucket, prefix),
				duration:    defaultSTSTTL,
			}
		}
		refresher := newSTSRefresher(provider, holder, defaultSTSTTL)
		if err := refresher.Start(context.Background()); err != nil {
			return fmt.Errorf("sts: %w", err)
		}
		defer refresher.Stop()
	}

	// Chroot to the company prefix BEFORE wrapping: the decorator inherits the
	// restricted namespace (defense in depth, decision 4.2).
	if st := metaCli.Chroot(meta.Background(), prefix); st != 0 {
		return st
	}

	// 3. Wrap with RenderMeta: local FEK unwrap/generate under the Company KEK.
	rm := meta.NewRenderMeta(metaCli, kek, kekVersion, format.UUID, companyID, prefix)
	defer rm.WipeKeys() // zero KEK + cached FEKs on every exit path (NFR-SEC-5)

	// D8: let the backend compact encrypted chunks by resolving FEKs locally.
	// Without this hook baseMeta skips them fail-closed (design 5.3).
	if setter, ok := metaCli.(interface {
		SetFileKeyResolver(func(ctx meta.Context, inode meta.Ino) ([]byte, string, uint32, error))
	}); ok {
		setter.SetFileKeyResolver(rm.ResolveFileKey)
	} else {
		logger.Warnf("meta backend %T does not support SetFileKeyResolver; encrypted chunks will not be compacted", metaCli)
	}

	logger.Infof("JuiceFS version %s", version.Version())
	registerer, registry := wrapRegister(c, mp, format.Name)
	store := chunk.NewCachedStore(blob, *chunkConf, registerer)
	registerMetaMsg(rm, store, chunkConf)

	if err = rm.NewSession(true); err != nil {
		logger.Fatalf("new session: %s", err)
	}
	rm.OnReload(func(fmt *meta.Format) {
		updateFormat(c)(fmt)
		store.UpdateLimit(fmt.UploadLimit, fmt.DownloadLimit)
	})
	v := vfs.NewVFS(vfsConf, rm, store, registerer, registry)
	installHandler(rm, mp, v, blob)
	v.UpdateFormat = updateFormat(c)
	initBackgroundTasks(c, vfsConf, metaConf, rm, blob, registerer, registry)
	renderPreMountSetup()
	mountMain(v, c)
	if err := v.FlushAll(""); err != nil {
		logger.Errorf("flush all delayed data: %s", err)
	}
	err = rm.CloseSession()
	rm.WipeKeys()
	object.Shutdown(blob)
	logger.Infof("The juicefs render-mount process exit successfully, mountpoint: %q", metaConf.MountPoint)
	return err
}
