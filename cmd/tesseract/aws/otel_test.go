// Copyright 2026 The Tessera authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func TestTraceFractionStartup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		invalid bool
	}{
		{name: "default"},
		{name: "zero", value: "0"},
		{name: "negative-zero", value: "-0"},
		{name: "smallest-positive", value: "5e-324"},
		{name: "fraction", value: "0.5"},
		{name: "below-one", value: "0.9999999999999999"},
		{name: "one", value: "1"},
		{name: "negative", value: "-5e-324", invalid: true},
		{name: "above-one", value: "1.0000000000000002", invalid: true},
		{name: "nan", value: "NaN", invalid: true},
		{name: "positive-infinity", value: "+Inf", invalid: true},
		{name: "negative-infinity", value: "-Inf", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTraceFractionStartupProcess$")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "OTEL_METRICS_EXPORTER=") && !strings.HasPrefix(entry, "TESSERACT_TRACE_FRACTION_TEST=") {
					command.Env = append(command.Env, entry)
				}
			}
			value := tc.value
			if tc.name == "default" {
				value = "default"
			}
			command.Env = append(command.Env, "OTEL_METRICS_EXPORTER=invalid", "TESSERACT_TRACE_FRACTION_TEST="+value)
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("startup timed out: %v: %s", ctx.Err(), output)
			}
			if err == nil {
				t.Fatalf("expected startup failure: %s", output)
			}
			expected := "Failed to create the metric reader"
			if tc.invalid {
				expected = "--trace_fraction must be a finite number between 0 and 1 inclusive"
			}
			if !strings.Contains(string(output), expected) {
				t.Fatalf("expected %q, got: %s", expected, output)
			}
		})
	}
}

func TestTraceFractionStartupProcess(t *testing.T) {
	value := os.Getenv("TESSERACT_TRACE_FRACTION_TEST")
	if value == "" {
		t.Skip("subprocess helper")
	}
	os.Args = []string{os.Args[0]}
	if value != "default" {
		os.Args = append(os.Args, "--trace_fraction="+value)
	}
	main()
}

func TestInitOTel(t *testing.T) {
	for _, scenario := range []string{"prometheus", "none", "sample-all", "invalid-exporter", "invalid-port", "occupied-port"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOTelProcess$")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "OTEL_") && !strings.HasPrefix(entry, "TESSERACT_OTEL_TEST=") {
					command.Env = append(command.Env, entry)
				}
			}
			command.Env = append(command.Env,
				"TESSERACT_OTEL_TEST="+scenario,
				"OTEL_METRICS_EXPORTER=none",
				"OTEL_TRACES_EXPORTER=none",
			)
			output, err := command.CombinedOutput()
			if strings.HasPrefix(scenario, "invalid-") || scenario == "occupied-port" {
				if err == nil || !strings.Contains(string(output), "Failed to create the metric reader") {
					t.Fatalf("expected metric reader startup failure, got %v: %s", err, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("telemetry check failed: %v: %s", err, output)
			}
		})
	}
}

func TestOTelProcess(t *testing.T) {
	scenario := os.Getenv("TESSERACT_OTEL_TEST")
	if scenario == "" {
		t.Skip("subprocess helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var address string
	if scenario == "prometheus" || scenario == "occupied-port" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address = listener.Addr().String()
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("OTEL_METRICS_EXPORTER", "prometheus")
		t.Setenv("OTEL_EXPORTER_PROMETHEUS_HOST", "127.0.0.1")
		t.Setenv("OTEL_EXPORTER_PROMETHEUS_PORT", port)
		if scenario == "occupied-port" {
			defer func() {
				if err := listener.Close(); err != nil {
					t.Error(err)
				}
			}()
		} else if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if scenario == "invalid-exporter" {
		t.Setenv("OTEL_METRICS_EXPORTER", "invalid")
	}
	if scenario == "invalid-port" {
		t.Setenv("OTEL_METRICS_EXPORTER", "prometheus")
		t.Setenv("OTEL_EXPORTER_PROMETHEUS_PORT", "invalid-port")
	}
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=test")
	traceFraction := 0.0
	if scenario == "sample-all" {
		traceFraction = 1
	}
	shutdown := initOTel(ctx, traceFraction, "example.com/testlog")
	defer shutdown(ctx)

	tracer := otel.Tracer("aws-otel-test")
	_, ordinary := tracer.Start(ctx, "ordinary")
	if ordinary.SpanContext().IsSampled() != (traceFraction == 1) {
		t.Error("ordinary span sampling does not match trace_fraction")
	}
	ordinary.End()
	_, periodic := tracer.Start(ctx, "periodic", trace.WithAttributes(attribute.Bool("tessera.periodic", true)))
	if !periodic.SpanContext().IsSampled() {
		t.Error("periodic root span was not sampled")
	}
	periodic.End()

	if scenario != "prometheus" {
		return
	}
	counter, err := otel.Meter("aws-otel-test").Int64Counter("aws_otel_test_requests")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(ctx, 1)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("scrape status: %s", response.Status)
	}
	for _, expected := range []string{"aws_otel_test_requests_total", "go_goroutine", `service_name="example.com/testlog"`, `service_namespace="tesseract"`, `deployment_environment="test"`} {
		if !strings.Contains(string(body), expected) {
			t.Errorf("scrape missing %q", expected)
		}
	}
	shutdown(ctx)
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
		t.Error("Prometheus listener still accepts connections after shutdown")
	}
}
