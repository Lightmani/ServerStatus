//go:build !linux

// Package collector gathers host metrics from /proc and /sys.
package collector

import (
	"errors"
	"runtime"

	"github.com/lightmani/serverstatus/internal/protocol"
)

type Collector struct{}

func New() *Collector { return &Collector{} }

func (c *Collector) Collect() (*protocol.Report, error) {
	return nil, errors.New("metric collection is only implemented for Linux, not " + runtime.GOOS)
}
