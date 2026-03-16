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

package meta

import (
	"context"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"hash"
	"os"
	"sync"

	sarama "github.com/Shopify/sarama"
	"github.com/ThreeDotsLabs/watermill"
	kafka "github.com/ThreeDotsLabs/watermill-kafka/v2/pkg/kafka"
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/pkg/errors"
	"github.com/xdg/scram"
)

var kafkaLogger = utils.GetLogger("juicefs-kafka")

// KafkaConfig holds Kafka connection configuration
type KafkaConfig struct {
	Brokers            []string
	User               string
	Password           string
	Cert               string
	CertFile           string
	InsecureSkipVerify bool
}

// WatermillKafkaPublisher publishes events to Kafka using Watermill
type WatermillKafkaPublisher struct {
	publisher *kafka.Publisher
	eventBus  *cqrs.EventBus
	mu        sync.Mutex
}

// NewWatermillKafkaPublisher creates a new Kafka publisher using Watermill
func NewWatermillKafkaPublisher(brokers []string, topic string) (*WatermillKafkaPublisher, error) {
	return NewWatermillKafkaPublisherWithConfig(KafkaConfig{
		Brokers: brokers,
	}, topic)
}

// NewWatermillKafkaPublisherWithConfig creates a new Kafka publisher with custom config
func NewWatermillKafkaPublisherWithConfig(cfg KafkaConfig, topic string) (*WatermillKafkaPublisher, error) {
	if len(cfg.Brokers) == 0 {
		return nil, errors.New("no kafka brokers specified")
	}

	if topic == "" {
		return nil, errors.New("kafka topic is empty")
	}

	// Configure Sarama
	saramaConfig, err := newSaramaConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "create sarama config")
	}

	logger := kafkaLoggerAdapter{}

	// Create Kafka publisher
	publisher, err := kafka.NewPublisher(
		kafka.PublisherConfig{
			Brokers:               cfg.Brokers,
			Marshaler:             NewKafkaMessageMarshaller(),
			OverwriteSaramaConfig: saramaConfig,
		},
		logger,
	)
	if err != nil {
		return nil, errors.Wrap(err, "create kafka publisher")
	}

	marshaler := NewJSONMarshalerWithPartitionKey()
	eventBusConfig := cqrs.EventBusConfig{
		GeneratePublishTopic: func(params cqrs.GenerateEventPublishTopicParams) (string, error) {
			return topic, nil
		},
		Marshaler: marshaler,
		Logger:    logger,
	}
	eventBus, err := cqrs.NewEventBusWithConfig(
		publisher,
		eventBusConfig,
	)
	if err != nil {
		return nil, errors.Wrap(err, "cannot create event bus")
	}

	return &WatermillKafkaPublisher{
		publisher: publisher,
		eventBus:  eventBus,
	}, nil
}

// newSaramaConfig creates a Sarama configuration with optional SASL and TLS support
func newSaramaConfig(cfg KafkaConfig) (*sarama.Config, error) {
	conf := sarama.NewConfig()
	conf.Version = sarama.V3_0_0_0
	conf.Producer.Return.Successes = true
	conf.Producer.Return.Errors = true
	conf.Producer.RequiredAcks = sarama.WaitForAll
	conf.Producer.Compression = sarama.CompressionLZ4

	// Configure SASL authentication if user/password provided
	if cfg.User != "" && cfg.Password != "" {
		conf.ClientID = "juicefs-kafka-client"
		conf.Net.SASL.Enable = true
		conf.Net.SASL.Handshake = true
		conf.Net.SASL.User = cfg.User
		conf.Net.SASL.Password = cfg.Password
		conf.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient { return &XDGSCRAMClient{HashGeneratorFcn: SHA512} }
		conf.Net.SASL.Mechanism = sarama.SASLMechanism(sarama.SASLTypeSCRAMSHA512)
	}

	// Configure TLS if certificate provided
	var pemData []byte
	var err error
	if cfg.Cert != "" {
		pemData, err = base64.StdEncoding.DecodeString(cfg.Cert)
	} else if cfg.CertFile != "" {
		pemData, err = os.ReadFile(cfg.CertFile)
	}
	if err != nil {
		return nil, errors.Wrap(err, "read certificate")
	}

	if len(pemData) > 0 {
		certs := x509.NewCertPool()
		if !certs.AppendCertsFromPEM(pemData) {
			return nil, errors.New("failed to parse certificate")
		}
		conf.Net.TLS.Enable = true
		conf.Net.TLS.Config = &tls.Config{
			InsecureSkipVerify: cfg.InsecureSkipVerify,
			RootCAs:            certs,
		}
	}

	return conf, nil
}

// Publish publishes an event to Kafka
func (p *WatermillKafkaPublisher) Publish(ctx context.Context, event *JuiceFsEvent) error {
	if err := p.eventBus.Publish(ctx, event); err != nil {
		return errors.Wrap(err, "publish to kafka")
	}

	return nil
}

// Close closes the publisher
func (p *WatermillKafkaPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.publisher != nil {
		return p.publisher.Close()
	}
	return nil
}

// kafkaLoggerAdapter implements watermill.LoggerAdapter interface
type kafkaLoggerAdapter struct{}

func (l kafkaLoggerAdapter) Debug(msg string, fields watermill.LogFields) {
	kafkaLogger.Debug(msg)
}

func (l kafkaLoggerAdapter) Info(msg string, fields watermill.LogFields) {
	kafkaLogger.Info(msg)
}

func (l kafkaLoggerAdapter) Error(msg string, err error, fields watermill.LogFields) {
	if err != nil {
		kafkaLogger.Errorf("%s: %v", msg, err)
	} else {
		kafkaLogger.Error(msg)
	}
}

func (l kafkaLoggerAdapter) Trace(msg string, fields watermill.LogFields) {
	kafkaLogger.Debug(msg)
}

func (l kafkaLoggerAdapter) With(fields watermill.LogFields) watermill.LoggerAdapter {
	return l
}

// InitializeWatermillKafka sets up Kafka publisher and outbox consumer
func InitializeWatermillKafka(ctx context.Context, outbox *RedisOutbox, brokers []string, topic string) error {
	if !outbox.Enabled() {
		return nil
	}

	publisher, err := NewWatermillKafkaPublisher(brokers, topic)
	if err != nil {
		return errors.Wrap(err, "create kafka publisher")
	}

	// Initialize consumer group
	if err := outbox.InitConsumerGroup(ctx); err != nil {
		return errors.Wrap(err, "init consumer group")
	}

	// Start consumer
	if err := outbox.StartConsumer(publisher); err != nil {
		return errors.Wrap(err, "start consumer")
	}

	kafkaLogger.Infof("Kafka outbox initialized: brokers=%v, topic=%s", brokers, topic)
	return nil
}

// SCRAM-SHA512 implementation for SASL authentication
var SHA512 scram.HashGeneratorFcn = func() hash.Hash { return sha512.New() }

type XDGSCRAMClient struct {
	*scram.Client
	*scram.ClientConversation
	scram.HashGeneratorFcn
}

func (x *XDGSCRAMClient) Begin(userName, password, authzID string) (err error) {
	x.Client, err = x.HashGeneratorFcn.NewClient(userName, password, authzID)
	if err != nil {
		return err
	}
	x.ClientConversation = x.Client.NewConversation()
	return nil
}

func (x *XDGSCRAMClient) Step(challenge string) (response string, err error) {
	response, err = x.ClientConversation.Step(challenge)
	return
}

func (x *XDGSCRAMClient) Done() bool {
	return x.ClientConversation.Done()
}

const PartitionMetadataKey = "partition"

func NewKafkaMessageMarshaller() kafka.Marshaler {
	return kafka.NewWithPartitioningMarshaler(func(topic string, msg *message.Message) (string, error) {
		return msg.Metadata.Get(PartitionMetadataKey), nil
	})
}

type JSONMarshaler struct {
	cqrs.JSONMarshaler
}

func NewJSONMarshalerWithPartitionKey() JSONMarshaler {
	return JSONMarshaler{
		JSONMarshaler: cqrs.JSONMarshaler{
			GenerateName: func(v interface{}) string {
				switch v.(type) {
				case *JuiceFsEvent:
					return "JuiceFsEvent"
				default:
					return cqrs.FullyQualifiedStructName(v)
				}
			},
		},
	}
}

func (m JSONMarshaler) Marshal(v interface{}) (msg *message.Message, err error) {
	switch i := v.(type) {
	case *JuiceFsEvent:
		msg, err = m.JSONMarshaler.Marshal(v)
		if err != nil {
			return msg, err
		}
		msg.Metadata.Set(PartitionMetadataKey, i.Volume+":"+i.Subdir+i.Path)
	default:
		msg, err = m.JSONMarshaler.Marshal(v)
	}
	return msg, err
}

func (m JSONMarshaler) Name(v interface{}) string {
	switch v.(type) {
	case *JuiceFsEvent:
		return "JuiceFsEvent"
	default:
		return m.JSONMarshaler.Name(v)
	}
}

func (m JSONMarshaler) NameFromMessage(msg *message.Message) string {
	return m.JSONMarshaler.NameFromMessage(msg)
}
