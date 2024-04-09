// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package mezmoexporter // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/mezmoexporter"

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

type mezmoExporter struct {
	config          *Config
	settings        component.TelemetrySettings
	client          *http.Client
	userAgentString string
	log             *zap.Logger
	wg              sync.WaitGroup
	bytesPool       *sync.Pool
}

type mezmoLogLine struct {
	Timestamp int64             `json:"timestamp"`
	Line      string            `json:"line"`
	App       string            `json:"app"`
	Level     string            `json:"level"`
	Meta      map[string]string `json:"meta"`
}

type mezmoLogBody struct {
	Lines []mezmoLogLine `json:"lines"`
}

func newLogsExporter(config *Config, settings component.TelemetrySettings, buildInfo component.BuildInfo, logger *zap.Logger) *mezmoExporter {
	var e = &mezmoExporter{
		config:          config,
		settings:        settings,
		userAgentString: fmt.Sprintf("mezmo-otel-exporter/%s", buildInfo.Version),
		log:             logger,
		bytesPool: &sync.Pool{
			New: func() any {
				return bytes.NewBuffer(make([]byte, 1024))
			},
		},
	}
	return e
}

func (m *mezmoExporter) pushLogData(_ context.Context, ld plog.Logs) error {
	m.wg.Add(1)
	defer m.wg.Done()

	return m.logDataToMezmo(ld)
}

func (m *mezmoExporter) start(ctx context.Context, host component.Host) (err error) {
	m.client, err = m.config.ClientConfig.ToClientContext(ctx, host, m.settings)
	return err
}

func (m *mezmoExporter) stop(context.Context) (err error) {
	if m.client == nil {
		return nil
	}
	m.wg.Wait()
	m.client.CloseIdleConnections()
	m.client = nil
	return nil
}

func (m *mezmoExporter) logDataToMezmo(ld plog.Logs) error {
	var errs error

	var lines []mezmoLogLine

	// Convert the log resources to mezmo lines...
	resourceLogs := ld.ResourceLogs()
	for i := 0; i < resourceLogs.Len(); i++ {
		resource := resourceLogs.At(i).Resource()
		resourceHostName, hasResourceHostName := resource.Attributes().Get("host.name")
		scopeLogs := resourceLogs.At(i).ScopeLogs()

		for j := 0; j < scopeLogs.Len(); j++ {
			logs := scopeLogs.At(j).LogRecords()

			for k := 0; k < logs.Len(); k++ {
				log := logs.At(k)

				// Convert Attributes to meta fields being mindful of the maxMetaDataSize restriction
				attrs := map[string]string{}
				if hasResourceHostName {
					attrs["hostname"] = resourceHostName.AsString()
				}

				if traceID := log.TraceID(); !traceID.IsEmpty() {
					attrs["trace.id"] = hex.EncodeToString(traceID[:])
				}

				if spanID := log.SpanID(); !spanID.IsEmpty() {
					attrs["span.id"] = hex.EncodeToString(spanID[:])
				}

				log.Attributes().Range(func(k string, v pcommon.Value) bool {
					attrs[k] = truncateString(v.Str(), maxMetaDataSize)
					return true
				})

				s, _ := log.Attributes().Get("appname")
				app := s.Str()

				tstamp := log.Timestamp().AsTime().UTC().UnixMilli()
				if tstamp == 0 {
					tstamp = time.Now().UTC().UnixMilli()
				}

				logLevel := truncateString(log.SeverityText(), maxLogLevelLen)
				if logLevel == "" {
					logLevel = "info"
				}

				line := mezmoLogLine{
					Timestamp: tstamp,
					Line:      truncateString(log.Body().Str(), maxMessageSize),
					App:       truncateString(app, maxAppnameLen),
					Level:     logLevel,
					Meta:      attrs,
				}
				lines = append(lines, line)
			}
		}
	}

	// Send them to Mezmo in batches < 10MB in size
	b := m.bytesPool.Get().(*bytes.Buffer)
	defer m.bytesPool.Put(b)
	b.Reset()
	b.WriteString("{\"lines\": [")

	var lineBytes []byte
	for i, line := range lines {
		if i > 0 {
			b.WriteRune(',')
		}
		if lineBytes, errs = json.Marshal(line); errs != nil {
			return fmt.Errorf("error Creating JSON payload: %w", errs)
		}

		var newBufSize = b.Len() + len(lineBytes)
		if newBufSize >= maxBodySize-2 {
			b.WriteString("]}")

			if errs = m.sendLinesToMezmo(b); errs != nil {
				return errs
			}
			b.Reset()
			b.WriteString("{\"lines\": [")
		}

		b.Write(lineBytes)

	}

	b.WriteString("]}")
	return m.sendLinesToMezmo(b)
}

func (m *mezmoExporter) sendLinesToMezmo(b *bytes.Buffer) (errs error) {
	var r io.Reader
	if m.config.Compression {
		buf := m.bytesPool.Get().(*bytes.Buffer)
		defer m.bytesPool.Put(buf)
		buf.Reset()
		w := gzip.NewWriter(buf)
		if _, err := w.Write(b.Bytes()); err != nil {
			return fmt.Errorf("failed to compress log data: %w", err)
		}
		_ = w.Close()
		r = buf
	} else {
		r = b
	}
	req, _ := http.NewRequest("POST", m.config.IngestURL, r)
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("User-Agent", m.userAgentString)
	req.Header.Add("apikey", string(m.config.IngestKey))
	if m.config.Compression {
		req.Header.Add("Content-Encoding", "gzip")
	}

	var res *http.Response
	if res, errs = m.client.Do(req); errs != nil {
		return fmt.Errorf("failed to POST log to Mezmo: %w", errs)
	}
	if res.StatusCode >= 400 {
		m.log.Error(fmt.Sprintf("got http status (%s): %s", req.URL.Path, res.Status))
		if checkLevel := m.log.Check(zap.DebugLevel, "http response"); checkLevel != nil {
			responseBody, _ := io.ReadAll(res.Body)
			checkLevel.Write(zap.String("response", string(responseBody)))
		}
	}

	return res.Body.Close()
}
