/*
 * JuiceFS, Copyright 2021 Juicedata, Inc.
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
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/urfave/cli/v2"
)

func cmdOutbox() *cli.Command {
	return &cli.Command{
		Name:     "outbox",
		Category: "OUTBOX",
		Usage:    "Consume filesystem events from Redis stream and publish to Kafka",
		Description: `
This command starts a consumer that reads filesystem events from a Redis stream
and publishes them to a Kafka topic. It should be run separately from the mount command.

Examples:
   # Consume from default stream with Kafka
   $ juicefs outbox redis://localhost --kafka-brokers kafka:9092

   # Custom stream and topic
   $ juicefs outbox redis://localhost \
       --outbox-stream my-stream \
       --group my-consumer-group \
       --kafka-brokers kafka1:9092,kafka2:9092 \
       --kafka-topic custom.events

   # With retry configuration
   $ juicefs outbox redis://localhost \
       --kafka-brokers kafka:9092 \
       --max-retries 5 \
       --trim-max-len 5000`,
		ArgsUsage: "REDIS-URL",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "outbox-stream",
				Value:   "juicefs:outbox",
				Usage:   "Redis stream name to consume from",
				EnvVars: []string{"JFS_OUTBOX_STREAM"},
			},
			&cli.StringFlag{
				Name:    "group",
				Value:   "meta-proxy-outbox",
				Usage:   "Redis consumer group name",
				EnvVars: []string{"JFS_OUTBOX_GROUP"},
			},
			&cli.StringFlag{
				Name:    "kafka-brokers",
				Usage:   "Comma-separated list of Kafka brokers (e.g., kafka1:9092,kafka2:9092), required for sink kafka",
				EnvVars: []string{"JFS_KAFKA_BROKERS"},
			},
			&cli.StringFlag{
				Name:    "kafka-topic",
				Value:   "juicefs.events",
				Usage:   "Kafka topic to publish events to",
				EnvVars: []string{"JFS_KAFKA_TOPIC"},
			},
			&cli.StringFlag{
				Name:    "kafka-user",
				Usage:   "Kafka SASL username for authentication",
				EnvVars: []string{"JFS_KAFKA_USER"},
			},
			&cli.StringFlag{
				Name:    "kafka-password",
				Usage:   "Kafka SASL password for authentication",
				EnvVars: []string{"JFS_KAFKA_PASSWORD"},
			},
			&cli.StringFlag{
				Name:    "kafka-cert",
				Usage:   "Kafka TLS certificate (base64 encoded)",
				EnvVars: []string{"JFS_KAFKA_CERT"},
			},
			&cli.StringFlag{
				Name:    "kafka-cert-file",
				Usage:   "Path to Kafka TLS certificate file",
				EnvVars: []string{"JFS_KAFKA_CERT_FILE"},
			},
			&cli.BoolFlag{
				Name:    "kafka-insecure-skip-verify",
				Usage:   "Skip TLS certificate verification for Kafka",
				EnvVars: []string{"JFS_KAFKA_INSECURE_SKIP_VERIFY"},
			},
			&cli.IntFlag{
				Name:    "max-retries",
				Value:   10,
				Usage:   "Maximum number of retries before moving message to dead-letter queue",
				EnvVars: []string{"JFS_OUTBOX_MAX_RETRIES"},
			},
			&cli.Int64Flag{
				Name:    "trim-max-len",
				Value:   10000,
				Usage:   "Maximum length of the Redis stream (0 to disable trimming)",
				EnvVars: []string{"JFS_OUTBOX_TRIM_MAX_LEN"},
			},
			&cli.StringFlag{
				Name:    "health-addr",
				Value:   ":9100",
				Usage:   "Address for the health check HTTP endpoint (empty to disable)",
				EnvVars: []string{"JFS_OUTBOX_HEALTH_ADDR"},
			},
			&cli.StringFlag{
				Name:    "sink",
				Value:   "kafka",
				Usage:   "Sink type: kafka or s3",
				EnvVars: []string{"JFS_OUTBOX_SINK"},
			},
			&cli.StringFlag{
				Name:    "s3-endpoint",
				Usage:   "S3 endpoint URL, required for sink s3 (e.g., http://minio:9000)",
				EnvVars: []string{"JFS_S3_ENDPOINT"},
			},
			&cli.StringFlag{
				Name:    "s3-bucket",
				Usage:   "S3 bucket name, required for sink s3",
				EnvVars: []string{"JFS_S3_BUCKET"},
			},
			&cli.StringFlag{
				Name:    "s3-access-key",
				Usage:   "S3 access key",
				EnvVars: []string{"JFS_S3_ACCESS_KEY"},
			},
			&cli.StringFlag{
				Name:    "s3-secret-key",
				Usage:   "S3 secret key",
				EnvVars: []string{"JFS_S3_SECRET_KEY"},
			},
			&cli.StringFlag{
				Name:    "s3-session-token",
				Usage:   "S3 session token",
				EnvVars: []string{"JFS_S3_SESSION_TOKEN"},
			},
			&cli.StringFlag{
				Name:    "s3-prefix",
				Value:   "events/",
				Usage:   "Object key prefix for events in the bucket",
				EnvVars: []string{"JFS_S3_PREFIX"},
			},
			&cli.StringFlag{
				Name:    "s3-region",
				Usage:   "S3 region (optional)",
				EnvVars: []string{"JFS_S3_REGION"},
			},
			&cli.StringFlag{
				Name:    "s3-storage",
				Value:   "minio",
				Usage:   "Object storage driver name for object.CreateStorage",
				EnvVars: []string{"JFS_S3_STORAGE"},
			},
			&cli.StringFlag{
				Name:    "s3-spill-dir",
				Usage:   "Local spill directory; empty disables spill",
				EnvVars: []string{"JFS_S3_SPILL_DIR"},
			},
			&cli.Int64Flag{
				Name:    "s3-spill-max-size",
				Value:   1073741824,
				Usage:   "Maximum total size of spill files in bytes",
				EnvVars: []string{"JFS_S3_SPILL_MAX_SIZE"},
			},
			&cli.DurationFlag{
				Name:    "reclaim-min-idle",
				Value:   time.Minute,
				Usage:   "How long a message must stay pending before it is reclaimed via XAutoClaim",
				EnvVars: []string{"JFS_RECLAIM_MIN_IDLE"},
			},
		},
		Action: outboxConsumer,
	}
}

// validateSinkConfig checks flag consistency for the selected sink kind:
// kafka requires --kafka-brokers, s3 requires --s3-endpoint and --s3-bucket,
// any other value is rejected. Pure function for testability.
func validateSinkConfig(sink, kafkaBrokers, s3Endpoint, s3Bucket string) error {
	switch sink {
	case "kafka":
		if kafkaBrokers == "" {
			return fmt.Errorf("kafka-brokers is required for sink kafka")
		}
	case "s3":
		if s3Endpoint == "" {
			return fmt.Errorf("s3-endpoint is required for sink s3")
		}
		if s3Bucket == "" {
			return fmt.Errorf("s3-bucket is required for sink s3")
		}
	default:
		return fmt.Errorf("unknown sink %q (supported: kafka, s3)", sink)
	}
	return nil
}

func outboxConsumer(c *cli.Context) error {
	setup(c, 1)

	redisURL := c.Args().First()
	if redisURL == "" {
		logger.Fatalf("Redis URL is required")
	}

	sinkType := c.String("sink")
	kafkaBrokersStr := c.String("kafka-brokers")
	if err := validateSinkConfig(sinkType, kafkaBrokersStr, c.String("s3-endpoint"), c.String("s3-bucket")); err != nil {
		logger.Fatalf("%v", err)
	}

	kafkaBrokers := strings.Split(kafkaBrokersStr, ",")
	for i := range kafkaBrokers {
		kafkaBrokers[i] = strings.TrimSpace(kafkaBrokers[i])
	}

	// Create Redis client with proper configuration (same as meta)
	rdb, err := meta.CreateRedisClient(redisURL)
	if err != nil {
		logger.Fatalf("Failed to create Redis client: %v", err)
	}
	defer rdb.Close()

	// Test Redis connection
	if err := rdb.Ping(c.Context).Err(); err != nil {
		logger.Fatalf("Failed to connect to Redis: %v", err)
	}
	logger.Infof("Connected to Redis: %s", utils.RemovePassword(redisURL))

	// Create outbox config
	outboxCfg := meta.OutboxConfig{
		Enabled:        true,
		StreamName:     c.String("outbox-stream"),
		ConsumerGroup:  c.String("group"),
		KafkaBrokers:   kafkaBrokers,
		KafkaTopic:     c.String("kafka-topic"),
		MaxRetries:     c.Int("max-retries"),
		TrimMaxLen:     c.Int64("trim-max-len"),
		ReclaimMinIdle: c.Duration("reclaim-min-idle"),
	}

	// Create outbox
	outbox := meta.NewRedisOutbox(rdb, outboxCfg)

	// Initialize consumer group
	if err := outbox.InitConsumerGroup(c.Context); err != nil {
		logger.Fatalf("Failed to initialize consumer group: %v", err)
	}

	// Create the sink (Kafka publisher or S3 object storage)
	var sink meta.EventSink
	var sinkDesc string
	switch sinkType {
	case "kafka":
		kafkaCfg := meta.KafkaConfig{
			Brokers:            kafkaBrokers,
			User:               c.String("kafka-user"),
			Password:           c.String("kafka-password"),
			Cert:               c.String("kafka-cert"),
			CertFile:           c.String("kafka-cert-file"),
			InsecureSkipVerify: c.Bool("kafka-insecure-skip-verify"),
		}
		publisher, err := meta.NewWatermillKafkaPublisherWithConfig(kafkaCfg, outboxCfg.KafkaTopic)
		if err != nil {
			logger.Fatalf("Failed to create Kafka publisher: %v", err)
		}
		defer publisher.Close()
		sink = publisher
		sinkDesc = fmt.Sprintf("kafka=%v, topic=%s", kafkaBrokers, outboxCfg.KafkaTopic)
	case "s3":
		endpoint := strings.TrimSuffix(c.String("s3-endpoint"), "/") + "/" + c.String("s3-bucket")
		if region := c.String("s3-region"); region != "" {
			// newMinio in pkg/object reads MINIO_REGION at storage construction
			// time, so the region must be set before CreateStorage
			os.Setenv("MINIO_REGION", region)
		}
		storage, err := object.CreateStorage(c.String("s3-storage"), endpoint, c.String("s3-access-key"), c.String("s3-secret-key"), c.String("s3-session-token"))
		if err != nil {
			logger.Fatalf("Failed to create object storage: %v", err)
		}
		s3Sink, err := meta.NewS3Sink(meta.S3SinkConfig{
			Prefix:       c.String("s3-prefix"),
			SpillDir:     c.String("s3-spill-dir"),
			SpillMaxSize: c.Int64("s3-spill-max-size"),
		}, storage)
		if err != nil {
			logger.Fatalf("Failed to create S3 sink: %v", err)
		}
		sink = s3Sink
		sinkDesc = fmt.Sprintf("s3=%s (storage=%s)", endpoint, c.String("s3-storage"))
	}

	// Start consumer
	if err := outbox.StartConsumer(sink); err != nil {
		logger.Fatalf("Failed to start consumer: %v", err)
	}
	logger.Infof("Outbox consumer started: stream=%s, group=%s, sink=%s",
		outboxCfg.StreamName, outboxCfg.ConsumerGroup, sinkDesc)

	// Start health check HTTP server
	var srv *http.Server
	if healthAddr := c.String("health-addr"); healthAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			healthy := outbox.IsHealthy()
			if hc, ok := sink.(meta.SinkHealthChecker); ok && hc.Health() != nil {
				healthy = false
			}
			if healthy {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("unhealthy"))
			}
		})
		srv = &http.Server{Addr: healthAddr, Handler: mux}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Fatalf("Health server failed: %v", err)
			}
		}()
		logger.Infof("Health endpoint listening on %s/health", healthAddr)
	}

	// Wait for interrupt signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("Shutting down outbox consumer...")
	if srv != nil {
		srv.Close()
	}
	outbox.StopConsumer()
	return nil
}
