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
	"fmt"
	"os"
	"time"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/urfave/cli/v2"
)

func cmdReencrypt() *cli.Command {
	return &cli.Command{
		Name:      "reencrypt",
		Action:    reencrypt,
		Category:  "ADMIN",
		Usage:     "Migrate legacy plaintext files to encrypted (background, resumable)",
		ArgsUsage: "META-URL [PATH]",
		Description: `
Walks the volume (or a subtree) and migrates legacy plaintext files to the
encrypted format: each chunk is merged into one slice under a fresh CEK and
swapped atomically (ReencryptChunk). Legacy files stay readable and writable
during the migration; new writes go encrypted. The command is idempotent and
resumable — already-migrated chunks are skipped, so it can be re-run after an
interruption.

With --rotate-cek it rotates the CEK of already-encrypted files instead (the
old S3 objects become GC candidates).

Examples:
# Migrate the whole volume (production: KEK via KeyManager + service identity)
$ juicefs reencrypt redis://localhost --company-id <uuid> --keymanager-service 10.0.0.1:9443

# Migrate a subtree with rate limits
$ juicefs reencrypt redis://localhost /renders --concurrency 8 --iops 200 --bandwidth 500

# Development: KEK from a file, no KeyManager
$ juicefs reencrypt redis://localhost --kek-file /tmp/kek.bin

# Rotate the CEK of encrypted files
$ juicefs reencrypt redis://localhost --rotate-cek --company-id <uuid> --keymanager-service 10.0.0.1:9443`,
		Flags: expandFlags(
			[]cli.Flag{
				&cli.StringFlag{
					Name:  "company-id",
					Usage: "company ID for KEK fetch (AAD component)",
				},
				&cli.StringFlag{
					Name:  "keymanager-service",
					Usage: "gRPC address of the DriveKeyManagerService (FetchCompanyKEK)",
				},
				&cli.StringFlag{
					Name:  "keymanager-tls-ca",
					Usage: "CA certificate for the KeyManager TLS transport",
				},
				&cli.StringFlag{
					Name:  "keymanager-server-name",
					Usage: "TLS server name for the KeyManager transport",
				},
				&cli.StringFlag{
					Name:  "iam-token-file",
					Usage: "dev only: file with the IAM token for FetchCompanyKEK (default: instance metadata)",
				},
				&cli.StringFlag{
					Name:  "kek-file",
					Usage: "dev only: path to a 32-byte Company KEK file (bypasses KeyManager)",
				},
				&cli.IntFlag{
					Name:  "concurrency",
					Value: 4,
					Usage: "number of files migrated in parallel",
				},
				&cli.Int64Flag{
					Name:  "iops",
					Value: 100,
					Usage: "max chunk-swap IOPS (0 = unlimited)",
				},
				&cli.Int64Flag{
					Name:  "bandwidth",
					Value: 0,
					Usage: "max merge bandwidth in MB/s (0 = unlimited)",
				},
				&cli.BoolFlag{
					Name:  "rotate-cek",
					Usage: "rotate the CEK of already-encrypted files instead of migrating legacy ones",
				},
			},
		),
	}
}

func reencrypt(c *cli.Context) error {
	setup(c, 2)
	addr := c.Args().Get(0)
	removePassword(addr)
	path := c.Args().Get(1)

	// 1. Company KEK: --kek-file (dev only) or FetchCompanyKEK by service identity.
	var kek []byte
	var kekVersion uint32
	companyID := c.String("company-id")
	if f := c.String("kek-file"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read KEK file: %w", err)
		}
		if len(b) != 32 {
			return fmt.Errorf("KEK file must be exactly 32 bytes, got %d", len(b))
		}
		kek, kekVersion = b, 1
		logger.Warnf("using Company KEK from %s (dev only)", f)
	} else {
		kmAddr := c.String("keymanager-service")
		if companyID == "" || kmAddr == "" {
			logger.Fatalf("reencrypt requires --company-id and --keymanager-service (or --kek-file for dev)")
		}
		var tp iamTokenProvider
		if f := c.String("iam-token-file"); f != "" {
			tp = &fileTokenProvider{path: f}
		} else {
			tp = &ycMetadataTokenProvider{audience: kmAddr}
		}
		nodeID := c.String("node-id")
		if nodeID == "" {
			nodeID, _ = os.Hostname()
		}
		km, err := meta.NewKeyManagerClient(kmAddr, "", "", c.String("keymanager-tls-ca"), c.String("keymanager-server-name"))
		if err != nil {
			return fmt.Errorf("keymanager client: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		kek, kekVersion, err = fetchCompanyKEK(ctx, km, tp, companyID, nodeID)
		cancel()
		if err != nil {
			_ = km.Close()
			return fmt.Errorf("fetch company KEK: %w", err)
		}
		if cerr := km.Close(); cerr != nil {
			logger.Warnf("close keymanager client: %s", cerr)
		}
		mlockKey(kek)
		logger.Infof("Company KEK fetched (version %d), company %s", kekVersion, companyID)
	}

	// 2. Metadata + object storage (direct access, like gc).
	metaConf := meta.DefaultConf()
	metaConf.NoBGJob = true
	m := meta.NewClient(addr, metaConf)
	format, err := m.Load(true)
	if err != nil {
		logger.Fatalf("load setting: %s", err)
	}
	if !format.EncryptionEnabled && !c.Bool("rotate-cek") {
		logger.Fatalf("volume %s is not encrypted; run `juicefs enable-encryption` first", format.Name)
	}

	blob, err := createStorage(*format)
	if err != nil {
		logger.Fatalf("object storage: %s", err)
	}
	logger.Infof("Data use %s", blob)
	chunkConf := *getDefaultChunkConf(format)
	chunkConf.CacheDir = "memory"
	store := chunk.NewCachedStore(blob, chunkConf, nil)

	// 3. Walk and migrate.
	w := newReencryptWorker(
		m, store, chunkConf, kek, kekVersion,
		format.UUID, companyID, c.Bool("rotate-cek"),
		c.Int("concurrency"), c.Int64("iops"), c.Int64("bandwidth"),
	)
	if err := w.run(c.Context, path); err != nil {
		return err
	}
	w.report()
	return nil
}
