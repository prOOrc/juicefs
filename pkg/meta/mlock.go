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

package meta

import (
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/prometheus/client_golang/prometheus"
)

// mlockFailures counts failed attempts to pin key material in RAM (NFR-SEC-3).
var mlockFailures = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "mlock_failures",
	Help: "number of failed attempts to mlock key material into RAM",
})

// fekMlockFailures counts failed attempts to pin FEK/KEK key material in RAM,
// on top of mlock_failures (task 10.4).
var fekMlockFailures = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "fek_mlock_failures",
	Help: "number of failed attempts to mlock FEK/KEK key material into RAM",
})

// mlockBestEffort pins key material in RAM so it cannot be swapped to disk.
// Failure is counted and logged, never fatal (NFR-SEC-3).
func mlockBestEffort(kind string, key []byte) {
	if err := utils.MlockPage(key); err != nil {
		if kind == "FEK" || kind == "KEK" {
			fekMlockFailures.Inc()
		}
		mlockFailures.Inc()
		logger.Warnf("mlock %s (best effort): %s", kind, err)
	}
}

// initMlockMetrics registers the mlock failure counter; double registration
// (e.g. in tests) is ignored.
func initMlockMetrics(reg prometheus.Registerer) {
	if reg == nil {
		return
	}
	_ = reg.Register(mlockFailures)
}

// initCryptoMetrics registers the encryption/offline metrics of task 10.4
// (defined next to their increment points); double registration is ignored.
func initCryptoMetrics(reg prometheus.Registerer) {
	if reg == nil {
		return
	}
	_ = reg.Register(fekCacheHits)
	_ = reg.Register(fekCacheMisses)
	_ = reg.Register(fekUnwrapLatency)
	_ = reg.Register(offlineEvents)
	_ = reg.Register(fekMlockFailures)
	_ = reg.Register(compactionsSkipped)
}
