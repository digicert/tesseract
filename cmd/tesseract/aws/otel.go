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
	"errors"
	"log/slog"
	"os"
	"time"

	t_otel "github.com/transparency-dev/tesseract/internal/otel"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
)

func initOTel(ctx context.Context, traceFraction float64, origin string) func(context.Context) {
	var shutdownFuncs []func(context.Context) error
	shutdown := func(ctx context.Context) {
		var err error
		for _, shutdownFunc := range shutdownFuncs {
			err = errors.Join(err, shutdownFunc(ctx))
		}
		shutdownFuncs = nil
		if err != nil {
			slog.ErrorContext(ctx, "OTel shutdown", slog.Any("error", err))
		}
	}

	mr, err := autoexport.NewMetricReader(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create the metric reader", slog.Any("error", err))
		os.Exit(1)
	}

	resources, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String(origin),
			semconv.ServiceNamespaceKey.String("tesseract"),
		),
		resource.WithFromEnv(),
	)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to detect resources", slog.Any("error", err))
		os.Exit(1)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(mr),
		sdkmetric.WithResource(resources),
	)
	shutdownFuncs = append(shutdownFuncs, mp.Shutdown)
	otel.SetMeterProvider(mp)

	te, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create the span exporter", slog.Any("error", err))
		os.Exit(1)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(t_otel.NewAttributeSampler([]string{"tessera.periodic"}, sdktrace.TraceIDRatioBased(traceFraction)))),
		sdktrace.WithBatcher(te),
		sdktrace.WithResource(resources),
	)
	shutdownFuncs = append(shutdownFuncs, tp.Shutdown)
	otel.SetTracerProvider(tp)

	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		slog.ErrorContext(ctx, "Failed to start exporting Go runtime metrics", slog.Any("error", err))
		os.Exit(1)
	}
	return shutdown
}

func shutdownOTelWithTimeout(shutdown func(context.Context), timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		shutdown(ctx)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		slog.Error("OTel shutdown timed out", slog.Any("error", ctx.Err()))
	}
}
