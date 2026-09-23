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
	"github.com/juicedata/juicefs/pkg/meta"
	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/urfave/cli/v2"
)

func cmdEnableEncryption() *cli.Command {
	return &cli.Command{
		Name:      "enable-encryption",
		Action:    enableEncryption,
		Category:  "ADMIN",
		Usage:     "Enable encryption on a volume (admin; idempotent)",
		ArgsUsage: "META-URL",
		Description: `
Provisions the company KEK if it does not exist yet and marks the volume as
encrypted (Format.EncryptionEnabled, Format.KEKVersion). Legacy files stay
readable as-is (plaintext passthrough); new files are created encrypted. Run
` + "`juicefs reencrypt`" + ` to migrate legacy data in the background.

Examples:
# Enable encryption on a volume (idempotent)
$ juicefs enable-encryption redis://localhost --company-id <uuid> --keymanager-service 10.0.0.1:9443`,
		Flags: expandFlags(
			[]cli.Flag{
				&cli.StringFlag{
					Name:  "company-id",
					Usage: "company ID (UUID) the volume belongs to",
				},
				&cli.StringFlag{
					Name:  "keymanager-service",
					Usage: "gRPC address of the DriveKeyManagerService",
				},
				&cli.StringFlag{
					Name:  "keymanager-tls-ca",
					Usage: "CA certificate for the KeyManager TLS transport",
				},
				&cli.StringFlag{
					Name:  "keymanager-server-name",
					Usage: "TLS server name for the KeyManager transport",
				},
			},
		),
	}
}

func enableEncryption(c *cli.Context) error {
	setup(c, 0)
	addr := c.Args().Get(0)
	removePassword(addr)
	companyID := c.String("company-id")
	kmAddr := c.String("keymanager-service")
	if companyID == "" || kmAddr == "" {
		logger.Fatalf("enable-encryption requires --company-id and --keymanager-service")
	}

	m := meta.NewClient(addr, meta.DefaultConf())
	format, err := m.Load(true)
	if err != nil {
		logger.Fatalf("load setting: %s", err)
	}
	if format.EncryptionEnabled {
		logger.Infof("encryption is already enabled on volume %s (KEK version %d)", format.Name, format.KEKVersion)
		return nil
	}

	km, err := meta.NewKeyManagerClient(kmAddr, "", "", c.String("keymanager-tls-ca"), c.String("keymanager-server-name"))
	if err != nil {
		logger.Fatalf("keymanager client: %s", err)
	}
	defer func() { _ = km.Close() }()
	resp, err := km.ProvisionCompanyKEK(c.Context, &kmpb.ProvisionCompanyKEKRequest{CompanyId: companyID})
	if err != nil {
		logger.Fatalf("ProvisionCompanyKEK: %s", err)
	}
	if resp.Created {
		logger.Infof("provisioned Company KEK v%d for %s", resp.KekVersion, companyID)
	} else {
		logger.Infof("Company KEK v%d already exists for %s", resp.KekVersion, companyID)
	}

	format.EncryptionEnabled = true
	format.KEKVersion = int(resp.KekVersion)
	if err = m.Init(format, false); err != nil {
		logger.Fatalf("update format: %s", err)
	}
	logger.Infof("encryption enabled on volume %s (KEK version %d)", format.Name, format.KEKVersion)
	return nil
}
