// pushgateway.go — PushgatewayEmitter: per-cell counters pushed to Prometheus Pushgateway over HTTP.
package validate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// PushgatewayEmitter sends per-cell counters to a Prometheus
// Pushgateway every Interval.  Soak runs that exceed an hour
// pair this with Grafana for live observability without a
// pull-scrape setup.
//
// We deliberately avoid the prometheus/client_golang dep —
// the pushgateway accepts plain Prometheus exposition format
// over HTTP PUT, which is a few lines of stdlib net/http.
// Operators wanting full client_golang plumbing wrap their
// own emitter outside this package.
type PushgatewayEmitter struct {
	URL      string        // base URL, e.g. http://pushgateway:9091
	Job      string        // prometheus `job` label, e.g. pg_hardstorage_validate
	Instance string        // prometheus `instance` label, e.g. <project>-<seed>
	Interval time.Duration // tick rate; default 30s
	Client   *http.Client  // optional override; default 5s timeout

	mu    sync.Mutex
	cells map[string]*cellMetrics
}

type cellMetrics struct {
	OS             string
	PG             string
	Iterations     int
	Backups        int
	BackupsFailed  int
	Restores       int
	RestoresFailed int
	Faults         int
	BytesWritten   int64
	LastEventUnix  int64
	Pass           int // 1 = pass, 0 = fail
}

// NewPushgatewayEmitter returns an emitter you start with Run.
// URL "" disables the emitter — Run returns immediately.
func NewPushgatewayEmitter(url, job, instance string) *PushgatewayEmitter {
	return &PushgatewayEmitter{
		URL: url, Job: job, Instance: instance,
		Interval: 30 * time.Second,
		Client:   &http.Client{Timeout: 5 * time.Second},
		cells:    map[string]*cellMetrics{},
	}
}

// OnEvent updates the in-memory counters per Event.  The soak
// orchestrator hands every Event to this method via the
// validate.Run OnEvent hook (composed alongside the NDJSON
// stream emitter).
func (e *PushgatewayEmitter) OnEvent(ev Event) {
	if e == nil || e.URL == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cm, ok := e.cells[ev.Cell]
	if !ok {
		cm = &cellMetrics{Pass: 1}
		e.cells[ev.Cell] = cm
	}
	cm.LastEventUnix = ev.At.Unix()
	switch ev.Op {
	case "iter_start":
		cm.Iterations = ev.Iteration
	case "backup_started":
		cm.Backups++
	case "backup_failed":
		cm.BackupsFailed++
	case "verify_started":
		cm.Restores++
	case "verify_failed":
		cm.RestoresFailed++
		cm.Pass = 0
	case "fault_apply":
		cm.Faults++
	case "setup_failed":
		cm.Pass = 0
	}
}

// AnnotateCellMetadata is called once per cell after Setup so
// the pushed labels include OS / PG (operators slice on these
// in Grafana).  Optional — without it the os / pg labels are
// empty strings.
func (e *PushgatewayEmitter) AnnotateCellMetadata(cell, os_, pg string) {
	if e == nil || e.URL == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cm, ok := e.cells[cell]
	if !ok {
		cm = &cellMetrics{Pass: 1}
		e.cells[cell] = cm
	}
	cm.OS = os_
	cm.PG = pg
}

// Run starts the periodic push loop.  Returns when ctx is
// cancelled.  Safe to call when URL is empty — exits immediately.
func (e *PushgatewayEmitter) Run(ctx context.Context) {
	if e == nil || e.URL == "" {
		return
	}
	tick := time.NewTicker(e.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			// Final flush so the last counters land before
			// shutdown.
			_ = e.Push(context.Background())
			return
		case <-tick.C:
			_ = e.Push(ctx)
		}
	}
}

// Push performs one PUT to the pushgateway.  Failures are
// returned but the soak driver swallows them — the run
// shouldn't fail because Grafana went away.
func (e *PushgatewayEmitter) Push(ctx context.Context) error {
	body := e.exposition()
	if body == "" {
		return nil
	}
	url := strings.TrimRight(e.URL, "/") +
		"/metrics/job/" + e.Job + "/instance/" + e.Instance
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url,
		strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	resp, err := e.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf := bytes.Buffer{}
		_, _ = io.Copy(&buf, resp.Body)
		return fmt.Errorf("pushgateway: %s (status=%d body=%s)",
			url, resp.StatusCode, truncate(buf.Bytes(), 256))
	}
	return nil
}

// exposition renders the in-memory counters in Prometheus
// text exposition format.  One metric per counter, labelled
// with cell + os + pg.
func (e *PushgatewayEmitter) exposition() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.cells) == 0 {
		return ""
	}
	cells := make([]string, 0, len(e.cells))
	for k := range e.cells {
		cells = append(cells, k)
	}
	sort.Strings(cells)

	var sb strings.Builder
	emitMetric := func(name string, get func(*cellMetrics) any) {
		fmt.Fprintf(&sb, "# TYPE %s gauge\n", name)
		for _, c := range cells {
			cm := e.cells[c]
			fmt.Fprintf(&sb,
				"%s{cell=%q,os=%q,pg=%q} %v\n",
				name, c, cm.OS, cm.PG, get(cm))
		}
	}
	emitMetric("pg_hardstorage_validate_iterations",
		func(cm *cellMetrics) any { return cm.Iterations })
	emitMetric("pg_hardstorage_validate_backups_total",
		func(cm *cellMetrics) any { return cm.Backups })
	emitMetric("pg_hardstorage_validate_backups_failed_total",
		func(cm *cellMetrics) any { return cm.BackupsFailed })
	emitMetric("pg_hardstorage_validate_restores_total",
		func(cm *cellMetrics) any { return cm.Restores })
	emitMetric("pg_hardstorage_validate_restores_failed_total",
		func(cm *cellMetrics) any { return cm.RestoresFailed })
	emitMetric("pg_hardstorage_validate_faults_applied_total",
		func(cm *cellMetrics) any { return cm.Faults })
	emitMetric("pg_hardstorage_validate_pass",
		func(cm *cellMetrics) any { return cm.Pass })
	emitMetric("pg_hardstorage_validate_last_event_unix",
		func(cm *cellMetrics) any { return cm.LastEventUnix })
	return sb.String()
}
