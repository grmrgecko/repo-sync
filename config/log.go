package config

import (
	"io"
	"os"

	log "github.com/sirupsen/logrus"
)

// logFiles holds the files opened by the previous SetupLogging call so a
// reload can close them once the new outputs are installed.
var logFiles []*os.File

// SetupLogging applies the configured log level, formatter, and outputs to
// the global logger. Unknown output paths are treated as files opened for
// appending; failures fall back to stderr so the server always logs.
func SetupLogging(lc LogConfig) {
	level, err := log.ParseLevel(lc.Level)
	if err != nil {
		level = log.InfoLevel
	}
	log.SetLevel(level)

	if lc.Type == "json" {
		log.SetFormatter(&log.JSONFormatter{})
	} else {
		log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	}

	var writers []io.Writer
	var files []*os.File
	for _, output := range lc.Outputs {
		if output == "console" {
			writers = append(writers, os.Stderr)
			continue
		}
		f, err := os.OpenFile(expandHome(output), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.WithError(err).WithField("output", output).Warn("Unable to open log output; skipping.")
			continue
		}
		writers = append(writers, f)
		files = append(files, f)
	}
	if len(writers) == 0 {
		writers = append(writers, os.Stderr)
	}
	log.SetOutput(io.MultiWriter(writers...))

	// Close the previous reload's files only after the new writer is
	// installed; the logger serializes writes, so none remain in flight.
	for _, f := range logFiles {
		f.Close()
	}
	logFiles = files
}
