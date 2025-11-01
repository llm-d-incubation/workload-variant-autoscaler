/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package collector

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
)

func TestMain(m *testing.M) {
	// Initialize logger for all collector tests (both standard and Ginkgo)
	_, err := logger.InitLogger()
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}

	// Run all tests
	code := m.Run()
	os.Exit(code)
}

func TestCollector(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Collector Suite")
}
