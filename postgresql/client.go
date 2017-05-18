package timescaledb

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"time"

	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/storage/remote"
)

// Config for the database
type Config struct {
	host                        string
	port                        int
	user                        string
	password                    string
	database                    string
	schema                      string
	table                       string
	pgPrometheusNormalize       bool
	pgPrometheusNormalizedTable string
	pgPrometheusKeepSamples     bool
}

// ParseFlags parses the configuration flags specific to PostgreSQL and TimescaleDB
func ParseFlags(cfg *Config) *Config {
	flag.StringVar(&cfg.host, "postgres-host", "localhost", "The PostgreSQL host")
	flag.IntVar(&cfg.port, "postgres-port", 5432, "The PostgreSQL port")
	flag.StringVar(&cfg.user, "postgres-user", "postgres", "The PostgreSQL user")
	flag.StringVar(&cfg.password, "postgres-password", "", "The PostgreSQL password")
	flag.StringVar(&cfg.database, "postgres-database", "postgres", "The PostgreSQL database")
	flag.StringVar(&cfg.schema, "postgres-schema", "", "The PostgreSQL schema")
	flag.StringVar(&cfg.table, "postgres-table", "samples", "The PostgreSQL table")
	flag.BoolVar(&cfg.pgPrometheusNormalize, "pg-prometheus-normalized-schema", false, "Insert metric samples into normalized pg_prometheus schema")
	flag.StringVar(&cfg.pgPrometheusNormalizedTable, "pg-prometheus-normalized-table-name", "metrics", "Name of the metrics table when using a normalized pg_prometheus schema")
	flag.BoolVar(&cfg.pgPrometheusKeepSamples, "pg-prometheus-keep-samples", true, "Keep raw samples when using normalized pg_prometheus schema")
	return cfg
}

// Client sends Prometheus samples to PostgreSQL
type Client struct {
	db  *sql.DB
	cfg *Config
}

// NewClient creates a new PostgreSQL client
func NewClient(cfg *Config) *Client {
	db, err := sql.Open("postgres", fmt.Sprintf("host=%v port=%v user=%v password=%v dbname=%v sslmode=disable connect_timeout=10",
		cfg.host, cfg.port, cfg.user, cfg.password, cfg.database))

	if err != nil {
		log.Fatal(err)
	}

	client := &Client{
		db:  db,
		cfg: cfg,
	}

	err = client.setupPgPrometheus()

	if err != nil {
		log.Fatal(err)
	}

	return client
}

func (c *Client) setupPgPrometheus() error {
	tx, err := c.db.Begin()

	if err != nil {
		return err
	}

	defer tx.Rollback()

	_, err = tx.Exec("CREATE EXTENSION IF NOT EXISTS pg_prometheus")

	if err != nil {
		return err
	}

	var rows *sql.Rows
	rows, err = tx.Query("SELECT create_prometheus_table($1, $2, normalized_tables => $3, keep_samples => $4)",
		c.cfg.table, c.cfg.pgPrometheusNormalizedTable, c.cfg.pgPrometheusNormalize, c.cfg.pgPrometheusKeepSamples)

	if err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return err
		}
		return nil
	}
	rows.Close()

	err = tx.Commit()

	if err != nil {
		return err
	}

	log.Infoln("Initialized pg_prometheus extension")

	return nil
}

func metricString(m model.Metric) string {
	metricName, hasName := m[model.MetricNameLabel]
	numLabels := len(m) - 1
	if !hasName {
		numLabels = len(m)
	}
	labelStrings := make([]string, 0, numLabels)
	for label, value := range m {
		if label != model.MetricNameLabel {
			labelStrings = append(labelStrings, fmt.Sprintf("%s=%q", label, value))
		}
	}

	switch numLabels {
	case 0:
		if hasName {
			return string(metricName)
		}
		return "{}"
	default:
		sort.Strings(labelStrings)
		return fmt.Sprintf("%s{%s}", metricName, strings.Join(labelStrings, ","))
	}
}

// Write implements the Writer interface and writes metric samples to the database
func (c *Client) Write(samples model.Samples) error {
	tx, err := c.db.Begin()

	if err != nil {
		return err
	}

	stmt, err := tx.Prepare(fmt.Sprintf("COPY \"%s\" FROM STDIN", c.cfg.table))

	if err != nil {
		return err
	}

	for _, sample := range samples {
		milliseconds := sample.Timestamp.UnixNano() / 1000000
		stmt.Exec(fmt.Sprintf("%v %v %v\n", metricString(sample.Metric), sample.Value, milliseconds))
	}

	err = stmt.Close()
	if err != nil {
		return err
	}

	err = tx.Commit()

	if err != nil {
		return err
	}
	return nil
}

type sampleLabels struct {
	JSON        []byte
	Map         map[string]string
	OrderedKeys []string
}

func createOrderedKeys(m *map[string]string) []string {
	keys := make([]string, 0, len(*m))
	for k := range *m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (l *sampleLabels) Scan(value interface{}) error {
	if value == nil {
		l = &sampleLabels{}
		return nil
	}

	switch t := value.(type) {
	case []uint8:
		m := make(map[string]string)
		err := json.Unmarshal(t, &m)

		if err != nil {
			return err
		}

		*l = sampleLabels{
			JSON:        t,
			Map:         m,
			OrderedKeys: createOrderedKeys(&m),
		}
		return nil
	}
	return fmt.Errorf("Invalid labels value %s", reflect.TypeOf(value))
}

func (l sampleLabels) String() string {
	return string(l.JSON)
}

func (l sampleLabels) key(extra string) string {
	// 0xff cannot cannot occur in valid UTF-8 sequences, so use it
	// as a separator here.
	separator := "\xff"
	pairs := make([]string, 0, len(l.Map)+1)
	pairs = append(pairs, extra+separator)

	for _, k := range l.OrderedKeys {
		pairs = append(pairs, k+separator+l.Map[k])
	}
	return strings.Join(pairs, separator)
}

func (l *sampleLabels) len() int {
	return len(l.OrderedKeys)
}

// Read implements the Reader interface and reads metrics samples from the database
func (c *Client) Read(req *remote.ReadRequest) (*remote.ReadResponse, error) {
	labelsToSeries := map[string]*remote.TimeSeries{}

	for _, q := range req.Queries {
		command, err := buildCommand(q, c.cfg.table)

		if err != nil {
			return nil, err
		}

		log.Debugf("Query '%v'", command)

		rows, err := c.db.Query(command)

		if err != nil {
			return nil, err
		}

		defer rows.Close()

		for rows.Next() {
			var (
				value  float64
				name   string
				labels sampleLabels
				time   time.Time
			)
			err := rows.Scan(&time, &name, &value, &labels)

			if err != nil {
				return nil, err
			}

			key := labels.key(name)
			ts, ok := labelsToSeries[key]

			if !ok {
				labelPairs := make([]*remote.LabelPair, 0, labels.len()+1)
				labelPairs = append(labelPairs, &remote.LabelPair{
					Name:  model.MetricNameLabel,
					Value: name,
				})

				for _, k := range labels.OrderedKeys {
					labelPairs = append(labelPairs, &remote.LabelPair{
						Name:  k,
						Value: labels.Map[k],
					})
				}

				ts = &remote.TimeSeries{
					Labels:  labelPairs,
					Samples: make([]*remote.Sample, 0, 100),
				}
				labelsToSeries[key] = ts
			}

			ts.Samples = append(ts.Samples, &remote.Sample{
				TimestampMs: time.UnixNano() / 1000000,
				Value:       value,
			})
		}

		err = rows.Err()

		if err != nil {
			return nil, err
		}
	}

	resp := remote.ReadResponse{
		Results: []*remote.QueryResult{
			{
				Timeseries: make([]*remote.TimeSeries, 0, len(labelsToSeries)),
			},
		},
	}
	for _, ts := range labelsToSeries {
		resp.Results[0].Timeseries = append(resp.Results[0].Timeseries, ts)
	}

	log.Debugf("Returned response with %v timeseries", len(labelsToSeries))

	return &resp, nil
}

// HealthCheck implements the healtcheck interface
func (c *Client) HealthCheck() error {
	rows, err := c.db.Query("SELECT 1")

	if err != nil {
		log.Debug("Health check error ", err)
		return err
	}

	rows.Close()
	return nil
}

func toTimestamp(milliseconds int64) time.Time {
	sec := milliseconds / 1000
	nsec := (milliseconds - (sec * 1000)) * 1000000
	return time.Unix(sec, nsec)
}

func buildCommand(q *remote.Query, table string) (string, error) {
	matchers := make([]string, 0, len(q.Matchers))
	// If we don't find a metric name matcher, query all metrics

	labelEqualPredicates := make(map[string]string)

	for _, m := range q.Matchers {
		if m.Name == model.MetricNameLabel {
			switch m.Type {
			case remote.MatchType_EQUAL:
				matchers = append(matchers, fmt.Sprintf("prom_name(sample) = '%s'", escapeSingleQuotes(m.Value)))
			case remote.MatchType_NOT_EQUAL:
				matchers = append(matchers, fmt.Sprintf("prom_name(sample) != '%s'", escapeSingleQuotes(m.Value)))
			case remote.MatchType_REGEX_MATCH:
				matchers = append(matchers, fmt.Sprintf("prom_name(sample) ~ '^%s$'", escapeSingleQuotes(m.Value)))
			case remote.MatchType_REGEX_NO_MATCH:
				matchers = append(matchers, fmt.Sprintf("prom_name(sample) !~ '^%s$'", escapeSingleQuotes(m.Value)))
			default:
				return "", fmt.Errorf("unknown metric name match type %v", m.Type)
			}
			continue
		}

		switch m.Type {
		case remote.MatchType_EQUAL:
			labelEqualPredicates[m.Name] = m.Value
		case remote.MatchType_NOT_EQUAL:
			matchers = append(matchers, fmt.Sprintf("prom_labels(sample)->>'%s' != '%q'", m.Name, escapeSingleQuotes(m.Value)))
		case remote.MatchType_REGEX_MATCH:
			matchers = append(matchers, fmt.Sprintf("prom_labels(sample)->>'%s' ~ '^%s$'", m.Name, escapeSingleQuotes(m.Value)))
		case remote.MatchType_REGEX_NO_MATCH:
			matchers = append(matchers, fmt.Sprintf("prom_labels(sample)->>'%s' !~ '^%s$'", m.Name, escapeSingleQuotes(m.Value)))
		default:
			return "", fmt.Errorf("unknown match type %v", m.Type)
		}
	}
	equalsPredicate := ""

	if len(labelEqualPredicates) > 0 {
		labelsJSON, err := json.Marshal(labelEqualPredicates)

		if err != nil {
			return "", err
		}
		equalsPredicate = fmt.Sprintf(" AND prom_labels(sample) @> '%s'", labelsJSON)
	}

	matchers = append(matchers, fmt.Sprintf("prom_time(sample) >= '%v'", toTimestamp(q.StartTimestampMs).Format(time.RFC3339)))
	matchers = append(matchers, fmt.Sprintf("prom_time(sample) <= '%v'", toTimestamp(q.EndTimestampMs).Format(time.RFC3339)))

	return fmt.Sprintf("SELECT prom_time(sample), prom_name(sample), prom_value(sample), prom_labels(sample) FROM %s WHERE %s %s",
		table, strings.Join(matchers, " AND "), equalsPredicate), nil
}

func escapeSingleQuotes(str string) string {
	return strings.Replace(str, `'`, `\'`, -1)
}

// Name identifies the client as a PostgreSQL client.
func (c Client) Name() string {
	return "PostgreSQL"
}

// Describe implements prometheus.Collector.
func (c *Client) Describe(ch chan<- *prometheus.Desc) {
}

// Collect implements prometheus.Collector.
func (c *Client) Collect(ch chan<- prometheus.Metric) {
	//ch <- c.ignoredSamples
}