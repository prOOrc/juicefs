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
	"fmt"
	"strings"
)

// NewMetaClient creates a Meta client for the given URI, returning an error
// instead of terminating the process on failure. It supports every registered
// meta driver and is safe for use as a library from another module. When conf is
// nil, DefaultConf() is used.
func NewMetaClient(uri string, conf *Config) (Meta, error) {
	if !strings.Contains(uri, "://") {
		uri = "redis://" + uri
	}
	p := strings.Index(uri, "://")
	if p < 0 {
		return nil, fmt.Errorf("invalid uri: %s", uri)
	}
	driver := uri[:p]
	if driver == "mysql" || driver == "postgres" {
		var err error
		if uri, err = setPasswordFromEnv(uri); err != nil {
			return nil, err
		}
	}
	f, ok := metaDrivers[driver]
	if !ok {
		return nil, fmt.Errorf("invalid meta driver: %s", driver)
	}
	if conf == nil {
		conf = DefaultConf()
	} else {
		conf.SelfCheck()
	}
	return f(driver, uri[p+3:], conf)
}
