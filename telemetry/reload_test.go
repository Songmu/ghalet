package telemetry

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	settings "github.com/Songmu/ghalet/config"
)

func TestPreparedMetrics(t *testing.T) {
	metricEnvironment(t)
	var requests, snapshots atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	m := NewRecorder()
	defer m.Close()
	config := settings.TelemetryConfig{
		Metrics: settings.MetricsConfig{Exporter: "otlp", Endpoint: server.URL + "/metrics"},
	}
	update, err := m.Prepare(t.Context(), "test", config, func() Snapshot {
		snapshots.Add(1)
		return Snapshot{Capacity: 1}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer update.Close()
	if err := update.generation.provider.ForceFlush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 || snapshots.Load() != 0 || m.StartRunner("target") != nil {
		t.Fatal("uncommitted configuration collected, exported, or enabled measurements")
	}
	if err := update.Close(); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 || snapshots.Load() != 0 {
		t.Fatal("discarded configuration collected or exported during shutdown")
	}

	for range 3 {
		update, err := m.Prepare(t.Context(), "test", config, func() Snapshot { return Snapshot{Capacity: 1} })
		if err != nil {
			t.Fatal(err)
		}
		old := m.Commit(update)
		if err := old.Close(); err != nil {
			t.Fatal(err)
		}
		var workers sync.WaitGroup
		for range 4 {
			workers.Go(func() {
				for range 100 {
					m.StartRunner("target").Ready()
				}
			})
		}
		if err := m.Commit(&Update{}).Close(); err != nil {
			t.Fatal(err)
		}
		workers.Wait()
	}
}
