package ovsdbstore

import (
	"io"
	"log"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
)

// libovsdb uses V3 for connection, leader and reconnect diagnostics. V4 logs
// complete transactions and V5 logs monitored rows and cache models, which can
// expose tenant topology and create unbounded journal volume.
const libovsdbLogVerbosity = 3

func newLibovsdbLogger(output io.Writer) logr.Logger {
	if output == nil {
		output = io.Discard
	}
	writer := log.New(output, "libovsdb: ", log.LstdFlags)
	return funcr.NewJSON(func(entry string) {
		writer.Print(entry)
	}, funcr.Options{Verbosity: libovsdbLogVerbosity})
}
