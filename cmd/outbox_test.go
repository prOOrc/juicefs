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
	"strings"
	"testing"
)

func TestValidateSinkFlags(t *testing.T) {
	cases := []struct {
		name         string
		sink         string
		kafkaBrokers string
		s3Endpoint   string
		s3Bucket     string
		wantErr      string // empty means no error expected
	}{
		{
			name:    "kafka with empty brokers",
			sink:    "kafka",
			wantErr: "kafka-brokers is required",
		},
		{
			name:         "kafka with brokers",
			sink:         "kafka",
			kafkaBrokers: "kafka1:9092,kafka2:9092",
		},
		{
			name:     "s3 without endpoint",
			sink:     "s3",
			s3Bucket: "events-bucket",
			wantErr:  "s3-endpoint",
		},
		{
			name:       "s3 without bucket",
			sink:       "s3",
			s3Endpoint: "http://minio:9000",
			wantErr:    "s3-bucket",
		},
		{
			name:       "s3 with endpoint and bucket",
			sink:       "s3",
			s3Endpoint: "http://minio:9000",
			s3Bucket:   "events-bucket",
		},
		{
			name:    "unknown sink",
			sink:    "webhook",
			wantErr: "unknown sink",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSinkConfig(c.sink, c.kafkaBrokers, c.s3Endpoint, c.s3Bucket)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("expected error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}
