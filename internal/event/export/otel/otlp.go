// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package otel

import (
	"fmt"
	"strconv"
)

// OTLP JSON types for telemetry export.

// intAsString is an int64 that marshals to/from a JSON string.
type intAsString int64

func (i intAsString) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, `"%d"`, i), nil
}

func (i *intAsString) UnmarshalJSON(data []byte) error {
	// Strip quotes if present.
	if len(data) >= 2 && data[0] == '"' && data[len(data)-1] == '"' {
		data = data[1 : len(data)-1]
	}
	v, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return err
	}
	*i = intAsString(v)
	return nil
}

// Common oltp types

// otlpResource corresponds to Resource.
type otlpResource struct {
	Attributes []otlpAttribute `json:"attributes"`
}

// https://github.com/open-telemetry/opentelemetry-proto/blob/v1.9.0/opentelemetry/proto/common/v1/common.proto#L76
type otlpScope struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// otlpAttribute corresponds to KeyValue.
type otlpAttribute struct {
	Key   string             `json:"key"`
	Value otlpAttributeValue `json:"value"`
}

// otlpAttributeValue corresponds to AnyValue.
// https://github.com/open-telemetry/opentelemetry-proto/blob/v1.9.0/opentelemetry/proto/common/v1/common.proto#L28
type otlpAttributeValue struct {
	StringValue *string      `json:"stringValue,omitempty"`
	IntValue    *intAsString `json:"intValue,omitempty"`
	DoubleValue *float64     `json:"doubleValue,omitempty"`
	BoolValue   *bool        `json:"boolValue,omitempty"`
}

// Trace types

// https://github.com/open-telemetry/opentelemetry-proto/blob/v1.9.0/opentelemetry/proto/collector/trace/v1/trace_service.proto#L34
type otlpTraceExportRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

// otlpResourceSpans corresponds to ResourceSpans.
type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

// otlpScopeSpans corresponds to ScopeSpans.
type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

// otlpSpan corresponds to Span.
// https://github.com/open-telemetry/opentelemetry-proto/blob/v1.9.0/opentelemetry/proto/trace/v1/trace.proto#L89
type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId,omitempty"`
	Name              string          `json:"name"`
	Kind              int             `json:"kind"`
	StartTimeUnixNano intAsString     `json:"startTimeUnixNano"`
	EndTimeUnixNano   intAsString     `json:"endTimeUnixNano"`
	Attributes        []otlpAttribute `json:"attributes,omitempty"`
	Status            otlpSpanStatus  `json:"status"`
}

// otlpSpanStatus corresponds to Status.
type otlpSpanStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

// Metrics types

// https://github.com/open-telemetry/opentelemetry-proto/blob/v1.9.0/opentelemetry/proto/collector/metrics/v1/metrics_service.proto#L34
type otlpMetricsRequest struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

// otlpResourceMetrics corresponds to ResourceMetrics.
type otlpResourceMetrics struct {
	Resource     otlpResource       `json:"resource"`
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}

// otlpScopeMetrics corresponds to ScopeMetrics.
type otlpScopeMetrics struct {
	Scope   otlpScope    `json:"scope"`
	Metrics []otlpMetric `json:"metrics"`
}

// otlpMetric corresponds to Metric.
// https://github.com/open-telemetry/opentelemetry-proto/blob/v1.9.0/opentelemetry/proto/metrics/v1/metrics.proto#L188
type otlpMetric struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Unit        string         `json:"unit,omitempty"`
	Gauge       *otlpGauge     `json:"gauge,omitempty"`
	Sum         *otlpSum       `json:"sum,omitempty"`
	Histogram   *otlpHistogram `json:"histogram,omitempty"`
}

// otlpGauge corresponds to Gauge.
type otlpGauge struct {
	DataPoints []otlpNumberDataPoint `json:"dataPoints"`
}

// otlpSum corresponds to Sum.
// https://github.com/open-telemetry/opentelemetry-proto/blob/v1.9.0/opentelemetry/proto/metrics/v1/metrics.proto#L240
type otlpSum struct {
	DataPoints             []otlpNumberDataPoint `json:"dataPoints"`
	AggregationTemporality int                   `json:"aggregationTemporality"`
	IsMonotonic            bool                  `json:"isMonotonic"`
}

// otlpHistogram corresponds to Histogram.
type otlpHistogram struct {
	DataPoints             []otlpHistogramDataPoint `json:"dataPoints"`
	AggregationTemporality int                      `json:"aggregationTemporality"`
}

// otlpNumberDataPoint corresponds to NumberDataPoint.
// https://github.com/open-telemetry/opentelemetry-proto/blob/v1.9.0/opentelemetry/proto/metrics/v1/metrics.proto#L385
type otlpNumberDataPoint struct {
	Attributes   []otlpAttribute `json:"attributes,omitempty"`
	TimeUnixNano intAsString     `json:"timeUnixNano"`
	AsInt        *intAsString    `json:"asInt,omitempty"`
	AsDouble     *float64        `json:"asDouble,omitempty"`
}

// otlpHistogramDataPoint corresponds to HistogramDataPoint.
// https://github.com/open-telemetry/opentelemetry-proto/blob/v1.9.0/opentelemetry/proto/metrics/v1/metrics.proto#L434
type otlpHistogramDataPoint struct {
	Attributes     []otlpAttribute `json:"attributes,omitempty"`
	TimeUnixNano   intAsString     `json:"timeUnixNano"`
	Count          intAsString     `json:"count"`
	Sum            *float64        `json:"sum,omitempty"`
	BucketCounts   []intAsString   `json:"bucketCounts"`
	ExplicitBounds []float64       `json:"explicitBounds"`
	Min            *float64        `json:"min,omitempty"`
	Max            *float64        `json:"max,omitempty"`
}
