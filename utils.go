package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
	"github.com/rs/zerolog/log"
)

// runCmd is a mockable variable for executing shell commands.
// Overriding this allows the test suite to verify shell commands without running them.
var runCmd = defaultRunCmd

func defaultRunCmd(name string, args ...string) error {
	log.Info().Msgf("Executing: %s (args redacted for safety)\n", name)
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runCmdWithOutput is a mockable variable for executing commands and capturing stdout.
var runCmdWithOutput = defaultRunCmdWithOutput

func defaultRunCmdWithOutput(name string, args ...string) ([]byte, error) {
	log.Info().Msgf("Executing (with output capture): %s (args redacted for safety)\n", name)
	cmd := exec.Command(name, args...)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

// runPipe is a mockable variable for piping standard output from one command to the input of another.
var runPipe = defaultRunPipe

func defaultRunPipe(cmd1 string, args1 []string, cmd2 string, args2 []string) error {
	log.Info().Msgf("Piping output from %s to %s...", cmd1, cmd2)
	c1 := exec.Command(cmd1, args1...)
	c2 := exec.Command(cmd2, args2...)

	c2.Stdin, _ = c1.StdoutPipe()
	c2.Stdout = os.Stdout
	c2.Stderr = os.Stderr

	if err := c2.Start(); err != nil {
		return err
	}
	if err := c1.Run(); err != nil {
		return err
	}
	return c2.Wait()
}

// getDirSize calculates the total size of a directory recursively in bytes
func getDirSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}

// injectPassword safely injects or overrides the password in a given database URI
func injectPassword(rawURI, password string) string {
	if rawURI == "" || password == "" {
		return rawURI
	}
	u, err := url.Parse(rawURI)
	if err != nil {
		return rawURI
	}
	username := ""
	if u.User != nil {
		username = u.User.Username()
	}
	u.User = url.UserPassword(username, password)
	return u.String()
}

// extractDBName safely parses a database URI and extracts the database name from the path
func extractDBName(rawURI string) string {
	if rawURI == "" {
		return ""
	}
	u, err := url.Parse(rawURI)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

// formatDuration converts a time.Duration into a human-readable string like "2 hours, 15 minutes, 3 seconds"
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	var parts []string
	if h > 0 {
		if h == 1 {
			parts = append(parts, "1 hour")
		} else {
			parts = append(parts, fmt.Sprintf("%d hours", h))
		}
	}
	if m > 0 {
		if m == 1 {
			parts = append(parts, "1 minute")
		} else {
			parts = append(parts, fmt.Sprintf("%d minutes", m))
		}
	}
	if s > 0 || len(parts) == 0 {
		if s == 1 {
			parts = append(parts, "1 second")
		} else {
			parts = append(parts, fmt.Sprintf("%d seconds", s))
		}
	}
	return strings.Join(parts, ", ")
}

// pushMetrics sends the pipeline completion status to a Prometheus Pushgateway
func pushMetrics(cfg SyncConfig, command string, durationSecs float64, success bool) {
	if cfg.PushgatewayURL == "" || cfg.DryRun {
		return
	}

	log.Info().Msgf("Pushing metrics to Pushgateway at %s...", cfg.PushgatewayURL)

	statusVal := 0.0
	if success {
		statusVal = 1.0
	}

	// Resolve the correct DB Host based on whether it's a backup or restore operation
	var targetURI string
	if command == "restore" {
		targetURI = cfg.DestURI
	} else {
		targetURI = cfg.SourceURI
	}
	dbHost := "unknown"
	if u, err := url.Parse(targetURI); err == nil && u.Hostname() != "" {
		dbHost = u.Hostname()
	}

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}

	// Apply the hostname label consistently across all metrics
	labels := prometheus.Labels{
		"instance": hostname,
	}

	statusGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "govuk_db_sync_status",
		Help:        "Status of the DB sync pipeline (1=success, 0=failure)",
		ConstLabels: labels,
	})
	statusGauge.Set(statusVal)

	durationGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "govuk_db_sync_duration_seconds",
		Help:        "Duration of the DB sync pipeline in seconds",
		ConstLabels: labels,
	})
	durationGauge.Set(durationSecs)

	timestampGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "govuk_db_sync_last_completed_timestamp_seconds",
		Help:        "Timestamp of the last completed DB sync pipeline",
		ConstLabels: labels,
	})
	timestampGauge.Set(float64(time.Now().Unix()))

	// Push groupings mapped accurately to GOV.UK dashboards
	pusher := push.New(cfg.PushgatewayURL, "db-sync").
		Grouping("database_engine", cfg.DBType).
		Grouping("database_instance", dbHost).
		Grouping("database_db_name", cfg.DBName).
		Grouping("operation", command).
		Collector(statusGauge).
		Collector(durationGauge).
		Collector(timestampGauge)

	if err := pusher.Push(); err != nil {
		log.Warn().Err(err).Msg("Failed to push metrics to Pushgateway")
	} else {
		log.Info().Msg("Successfully pushed metrics to Pushgateway.")
	}
}
