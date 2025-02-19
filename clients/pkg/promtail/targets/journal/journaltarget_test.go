//go:build linux && cgo
// +build linux,cgo

package journal

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-systemd/sdjournal"
	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/prometheus/model/relabel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"

	"github.com/grafana/loki/clients/pkg/promtail/client/fake"
	"github.com/grafana/loki/clients/pkg/promtail/positions"
	"github.com/grafana/loki/clients/pkg/promtail/scrapeconfig"
	"github.com/grafana/loki/clients/pkg/promtail/targets/testutils"
)

type mockJournalReader struct {
	config sdjournal.JournalReaderConfig
	t      *testing.T
}

func newMockJournalReader(c sdjournal.JournalReaderConfig) (journalReader, error) {
	return &mockJournalReader{config: c}, nil
}

func (r *mockJournalReader) Close() error {
	return nil
}

func (r *mockJournalReader) Follow(until <-chan time.Time, writer io.Writer) error {
	<-until
	return nil
}

func newMockJournalEntry(entry *sdjournal.JournalEntry) journalEntryFunc {
	return func(c sdjournal.JournalReaderConfig, cursor string) (*sdjournal.JournalEntry, error) {
		return entry, nil
	}
}

func (r *mockJournalReader) Write(fields map[string]string) {
	allFields := make(map[string]string, len(fields))
	for k, v := range fields {
		allFields[k] = v
	}

	ts := uint64(time.Now().UnixNano())

	_, err := r.config.Formatter(&sdjournal.JournalEntry{
		Fields:             allFields,
		MonotonicTimestamp: ts,
		RealtimeTimestamp:  ts,
	})
	assert.NoError(r.t, err)
}

func TestJournalTarget(t *testing.T) {
	w := log.NewSyncWriter(os.Stderr)
	logger := log.NewLogfmtLogger(w)

	testutils.InitRandom()
	dirName := "/tmp/" + testutils.RandName()
	positionsFileName := dirName + "/positions.yml"

	// Set the sync period to a really long value, to guarantee the sync timer
	// never runs, this way we know everything saved was done through channel
	// notifications when target.stop() was called.
	ps, err := positions.New(logger, positions.Config{
		SyncPeriod:    10 * time.Second,
		PositionsFile: positionsFileName,
	})
	if err != nil {
		t.Fatal(err)
	}

	client := fake.New(func() {})

	relabelCfg := `
- source_labels: ['__journal_code_file']
  regex: 'journaltarget_test\.go'
  action: 'keep'
- source_labels: ['__journal_code_file']
  target_label: 'code_file'`

	var relabels []*relabel.Config
	err = yaml.Unmarshal([]byte(relabelCfg), &relabels)
	require.NoError(t, err)

	registry := prometheus.NewRegistry()
	jt, err := journalTargetWithReader(NewMetrics(registry), logger, client, ps, "test", relabels,
		&scrapeconfig.JournalTargetConfig{}, newMockJournalReader, newMockJournalEntry(nil))
	require.NoError(t, err)

	r := jt.r.(*mockJournalReader)
	r.t = t

	for i := 0; i < 10; i++ {
		r.Write(map[string]string{
			"MESSAGE":   "ping",
			"CODE_FILE": "journaltarget_test.go",
		})
		assert.NoError(t, err)
	}
	require.NoError(t, jt.Stop())
	client.Stop()

	expectedMetrics := `# HELP promtail_journal_target_lines_total Total number of successful journal lines read
	# TYPE promtail_journal_target_lines_total counter
	promtail_journal_target_lines_total 10
	`

	if err := testutil.GatherAndCompare(registry,
		strings.NewReader(expectedMetrics)); err != nil {
		t.Fatalf("mismatch metrics: %v", err)
	}
	assert.Len(t, client.Received(), 10)
}

func TestJournalTargetParsingErrors(t *testing.T) {
	w := log.NewSyncWriter(os.Stderr)
	logger := log.NewLogfmtLogger(w)

	testutils.InitRandom()
	dirName := "/tmp/" + testutils.RandName()
	positionsFileName := dirName + "/positions.yml"

	// Set the sync period to a really long value, to guarantee the sync timer
	// never runs, this way we know everything saved was done through channel
	// notifications when target.stop() was called.
	ps, err := positions.New(logger, positions.Config{
		SyncPeriod:    10 * time.Second,
		PositionsFile: positionsFileName,
	})
	if err != nil {
		t.Fatal(err)
	}

	client := fake.New(func() {})

	// We specify no relabel rules, so that we end up with an empty labelset
	var relabels []*relabel.Config

	registry := prometheus.NewRegistry()
	jt, err := journalTargetWithReader(NewMetrics(registry), logger, client, ps, "test", relabels,
		&scrapeconfig.JournalTargetConfig{}, newMockJournalReader, newMockJournalEntry(nil))
	require.NoError(t, err)

	r := jt.r.(*mockJournalReader)
	r.t = t

	// No labels but correct message
	for i := 0; i < 10; i++ {
		r.Write(map[string]string{
			"MESSAGE":   "ping",
			"CODE_FILE": "journaltarget_test.go",
		})
		assert.NoError(t, err)
	}

	// No labels and no message
	for i := 0; i < 10; i++ {
		r.Write(map[string]string{
			"CODE_FILE": "journaltarget_test.go",
		})
		assert.NoError(t, err)
	}
	require.NoError(t, jt.Stop())
	client.Stop()

	expectedMetrics := `# HELP promtail_journal_target_lines_total Total number of successful journal lines read
	# TYPE promtail_journal_target_lines_total counter
	promtail_journal_target_lines_total 0
	# HELP promtail_journal_target_parsing_errors_total Total number of parsing errors while reading journal messages
	# TYPE promtail_journal_target_parsing_errors_total counter
	promtail_journal_target_parsing_errors_total{error="empty_labels"} 10
	promtail_journal_target_parsing_errors_total{error="no_message"} 10
	`

	if err := testutil.GatherAndCompare(registry,
		strings.NewReader(expectedMetrics)); err != nil {
		t.Fatalf("mismatch metrics: %v", err)
	}

	assert.Len(t, client.Received(), 0)
}

func TestJournalTarget_JSON(t *testing.T) {
	w := log.NewSyncWriter(os.Stderr)
	logger := log.NewLogfmtLogger(w)

	testutils.InitRandom()
	dirName := "/tmp/" + testutils.RandName()
	positionsFileName := dirName + "/positions.yml"

	// Set the sync period to a really long value, to guarantee the sync timer
	// never runs, this way we know everything saved was done through channel
	// notifications when target.stop() was called.
	ps, err := positions.New(logger, positions.Config{
		SyncPeriod:    10 * time.Second,
		PositionsFile: positionsFileName,
	})
	if err != nil {
		t.Fatal(err)
	}

	client := fake.New(func() {})

	relabelCfg := `
- source_labels: ['__journal_code_file']
  regex: 'journaltarget_test\.go'
  action: 'keep'
- source_labels: ['__journal_code_file']
  target_label: 'code_file'`

	var relabels []*relabel.Config
	err = yaml.Unmarshal([]byte(relabelCfg), &relabels)
	require.NoError(t, err)

	cfg := &scrapeconfig.JournalTargetConfig{JSON: true}

	jt, err := journalTargetWithReader(NewMetrics(prometheus.NewRegistry()), logger, client, ps, "test", relabels,
		cfg, newMockJournalReader, newMockJournalEntry(nil))
	require.NoError(t, err)

	r := jt.r.(*mockJournalReader)
	r.t = t

	for i := 0; i < 10; i++ {
		r.Write(map[string]string{
			"MESSAGE":     "ping",
			"CODE_FILE":   "journaltarget_test.go",
			"OTHER_FIELD": "foobar",
		})
		assert.NoError(t, err)

	}
	expectMsg := `{"CODE_FILE":"journaltarget_test.go","MESSAGE":"ping","OTHER_FIELD":"foobar"}`
	require.NoError(t, jt.Stop())
	client.Stop()

	assert.Len(t, client.Received(), 10)
	for i := 0; i < 10; i++ {
		require.Equal(t, expectMsg, client.Received()[i].Line)
	}
}

func TestJournalTarget_Since(t *testing.T) {
	w := log.NewSyncWriter(os.Stderr)
	logger := log.NewLogfmtLogger(w)

	testutils.InitRandom()
	dirName := "/tmp/" + testutils.RandName()
	positionsFileName := dirName + "/positions.yml"

	// Set the sync period to a really long value, to guarantee the sync timer
	// never runs, this way we know everything saved was done through channel
	// notifications when target.stop() was called.
	ps, err := positions.New(logger, positions.Config{
		SyncPeriod:    10 * time.Second,
		PositionsFile: positionsFileName,
	})
	if err != nil {
		t.Fatal(err)
	}

	client := fake.New(func() {})

	cfg := scrapeconfig.JournalTargetConfig{
		MaxAge: "4h",
	}

	jt, err := journalTargetWithReader(NewMetrics(prometheus.NewRegistry()), logger, client, ps, "test", nil,
		&cfg, newMockJournalReader, newMockJournalEntry(nil))
	require.NoError(t, err)

	r := jt.r.(*mockJournalReader)
	require.Equal(t, r.config.Since, -1*time.Hour*4)
	client.Stop()
}

func TestJournalTarget_Cursor_TooOld(t *testing.T) {
	w := log.NewSyncWriter(os.Stderr)
	logger := log.NewLogfmtLogger(w)

	testutils.InitRandom()
	dirName := "/tmp/" + testutils.RandName()
	positionsFileName := dirName + "/positions.yml"

	// Set the sync period to a really long value, to guarantee the sync timer
	// never runs, this way we know everything saved was done through channel
	// notifications when target.stop() was called.
	ps, err := positions.New(logger, positions.Config{
		SyncPeriod:    10 * time.Second,
		PositionsFile: positionsFileName,
	})
	if err != nil {
		t.Fatal(err)
	}
	ps.PutString("journal-test", "foobar")

	client := fake.New(func() {})

	cfg := scrapeconfig.JournalTargetConfig{}

	entryTs := time.Date(1980, time.July, 3, 12, 0, 0, 0, time.UTC)
	journalEntry := newMockJournalEntry(&sdjournal.JournalEntry{
		Cursor:            "foobar",
		Fields:            nil,
		RealtimeTimestamp: uint64(entryTs.UnixNano()),
	})

	jt, err := journalTargetWithReader(NewMetrics(prometheus.NewRegistry()), logger, client, ps, "test", nil,
		&cfg, newMockJournalReader, journalEntry)
	require.NoError(t, err)

	r := jt.r.(*mockJournalReader)
	require.Equal(t, r.config.Since, -1*time.Hour*7)
	client.Stop()
}

func TestJournalTarget_Cursor_NotTooOld(t *testing.T) {
	w := log.NewSyncWriter(os.Stderr)
	logger := log.NewLogfmtLogger(w)

	testutils.InitRandom()
	dirName := "/tmp/" + testutils.RandName()
	positionsFileName := dirName + "/positions.yml"

	// Set the sync period to a really long value, to guarantee the sync timer
	// never runs, this way we know everything saved was done through channel
	// notifications when target.stop() was called.
	ps, err := positions.New(logger, positions.Config{
		SyncPeriod:    10 * time.Second,
		PositionsFile: positionsFileName,
	})
	if err != nil {
		t.Fatal(err)
	}
	ps.PutString(positions.CursorKey("test"), "foobar")

	client := fake.New(func() {})

	cfg := scrapeconfig.JournalTargetConfig{}

	entryTs := time.Now().Add(-time.Hour)
	journalEntry := newMockJournalEntry(&sdjournal.JournalEntry{
		Cursor:            "foobar",
		Fields:            nil,
		RealtimeTimestamp: uint64(entryTs.UnixNano() / int64(time.Microsecond)),
	})

	jt, err := journalTargetWithReader(NewMetrics(prometheus.NewRegistry()), logger, client, ps, "test", nil,
		&cfg, newMockJournalReader, journalEntry)
	require.NoError(t, err)

	r := jt.r.(*mockJournalReader)
	require.Equal(t, r.config.Since, time.Duration(0))
	require.Equal(t, r.config.Cursor, "foobar")
	client.Stop()
}

func Test_MakeJournalFields(t *testing.T) {
	entryFields := map[string]string{
		"CODE_FILE":   "journaltarget_test.go",
		"OTHER_FIELD": "foobar",
		"PRIORITY":    "6",
	}
	receivedFields := makeJournalFields(entryFields)
	expectedFields := map[string]string{
		"__journal_code_file":        "journaltarget_test.go",
		"__journal_other_field":      "foobar",
		"__journal_priority":         "6",
		"__journal_priority_keyword": "info",
	}
	assert.Equal(t, expectedFields, receivedFields)
}
