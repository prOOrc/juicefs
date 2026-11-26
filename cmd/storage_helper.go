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

// Хелпер STS-подключения хранилища узла, общий для render-mount и
// clouddrive serve (design.md Integration Points: «Общая часть выделяется в
// хелпер cmd, чтобы mount и clouddrive не разошлись»). Файл платформо-
// нейтральный: render-mount собирается на всех ОС.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/urfave/cli/v2"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
)

// nodeSTSFlags — общий набор флагов STS-подключения узла (render-mount,
// clouddrive serve): определения совпадают с прежним набором render-mount,
// поэтому поведение и подсказки --help не меняются.
func nodeSTSFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:  "company-id",
			Usage: "company ID (UUID) of this node's company; used for FetchCompanyKEK and bound into the FEK AAD",
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
			Usage: "use short-lived STS storage credentials (AWS: AssumeRole with the node instance profile; YC: platform-issued STS via the KeyManager, authenticated by the node IAM token) instead of static keys in the volume format",
		},
		&cli.StringFlag{
			Name:  "sts-role-arn",
			Usage: "IAM role ARN to assume for storage access (required with --sts-enabled on AWS volumes)",
		},
	}
}

// stsNodeOptions — параметры STS-подключения, собранные из флагов команды
// (clouddrive serve/ls/hash: KeyManager и токен нужны только при
// --sts-enabled, поэтому собираются здесь, а не на верхнем уровне).
type stsNodeOptions struct {
	companyID  string
	kmsAddr    string
	tlsCA      string
	serverName string
	tokenFile  string
	nodeID     string
	roleARN    string
}

// stsNodeOptionsFrom читает STS-параметры из контекста команды; nil, когда
// --sts-enabled не задан (хранилище работает на статических ключах format).
func stsNodeOptionsFrom(c *cli.Context) *stsNodeOptions {
	if !c.Bool("sts-enabled") {
		return nil
	}
	return &stsNodeOptions{
		companyID:  c.String("company-id"),
		kmsAddr:    c.String("keymanager-service"),
		tlsCA:      c.String("keymanager-tls-ca"),
		serverName: c.String("keymanager-server-name"),
		tokenFile:  c.String("iam-token-file"),
		nodeID:     c.String("node-id"),
		roleARN:    c.String("sts-role-arn"),
	}
}

// openKeyManager открывает клиент KeyManager по параметрам STS (platform-STS
// путь; для AWS с AssumeRole клиент не нужен — вызовите только при нужде).
func openKeyManager(opts *stsNodeOptions) (meta.KeyManagerClient, iamTokenProvider, error) {
	if opts.companyID == "" {
		return nil, nil, errors.New("--company-id is required with --sts-enabled")
	}
	if opts.kmsAddr == "" {
		return nil, nil, errors.New("--keymanager-service is required with --sts-enabled")
	}
	km, err := meta.NewRenderKeyManagerClient(opts.kmsAddr, opts.tlsCA, opts.serverName)
	if err != nil {
		return nil, nil, err
	}
	var tp iamTokenProvider
	if opts.tokenFile != "" {
		tp = &fileTokenProvider{path: opts.tokenFile}
	} else {
		tp = &ycMetadataTokenProvider{audience: opts.kmsAddr}
	}
	return km, tp, nil
}

// startNodeSTS подключает обновляемые STS-креды к reloadable-хранилищу
// (render-mount и clouddrive serve, ADR-003; выбор по format.Storage):
// AWS — AssumeRole ролью узла с session policy по префиксу тома, остальные
// бэкенды — platform-issued STS через KeyManager (GetNodeSTSCredentials),
// аутентификация IAM-токеном узла. Первый набор кредов получается fail-fast.
// Возвращает запущенный refresher; KeyManager остаётся открытым, пока
// refresher жив (креды обновляются тем же каналом) — за его закрытие
// отвечает вызывающий.
func startNodeSTS(format *meta.Format, blob object.ObjectStorage, roleARN string, km meta.KeyManagerClient, tp iamTokenProvider, nodeID, companyID string) (*stsRefresher, error) {
	holder, ok := blob.(*storageHolder)
	if !ok {
		return nil, errors.New("sts: storage is not reloadable")
	}
	var provider stsCredentialsProvider
	switch format.Storage {
	case "aws":
		if roleARN == "" {
			return nil, errors.New("--sts-role-arn is required with --sts-enabled on AWS volumes")
		}
		provider = &awsAssumeRoleProvider{
			roleARN:     roleARN,
			sessionName: nodeID,
			bucket:      format.Bucket,
			prefix:      format.Name + "/", // chunk keys are <volume>/chunks/... (A6)
			duration:    defaultSTSTTL,
		}
	default:
		provider = &platformNodeSTSProvider{
			keyManager: km,
			token:      tp,
			nodeID:     nodeID,
			companyID:  companyID,
		}
	}
	refresher := newSTSRefresher(provider, holder, meta.DefaultSTSRefreshMargin)
	if err := refresher.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("sts: %w", err)
	}
	return refresher, nil
}

// defaultNodeID — идентификатор узла по умолчанию (hostname).
func defaultNodeID() string {
	nodeID, err := os.Hostname()
	if err != nil || nodeID == "" {
		return "unknown-node"
	}
	return nodeID
}
