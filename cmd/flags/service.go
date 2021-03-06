// Copyright (c) 2019 The Jaeger Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package flags

import (
	"flag"
	"io/ioutil"
	"os"
	"os/signal"
	"syscall"

	"github.com/pkg/errors"
	"github.com/spf13/viper"
	"github.com/uber/jaeger-lib/metrics"
	"go.uber.org/zap"
	"google.golang.org/grpc/grpclog"

	"github.com/jaegertracing/jaeger/pkg/healthcheck"
	pMetrics "github.com/jaegertracing/jaeger/pkg/metrics"
)

// Service represents an abstract Jaeger backend component with some basic shared functionality.
type Service struct {
	// AdminPort is the HTTP port number for admin server.
	AdminPort int

	// NoStorage indicates that storage type CLI flag is not applicable
	NoStorage bool

	// Admin is the admin server that hosts the health check and metrics endpoints.
	Admin *AdminServer

	// Logger is initialized after parsing Viper flags like --log-level.
	Logger *zap.Logger

	// MetricsFactory is the root factory without a namespace.
	MetricsFactory metrics.Factory

	signalsChannel chan os.Signal

	hcStatusChannel chan healthcheck.Status
}

// NewService creates a new Service.
func NewService(adminPort int) *Service {
	signalsChannel := make(chan os.Signal, 1)
	hcStatusChannel := make(chan healthcheck.Status)
	signal.Notify(signalsChannel, os.Interrupt, syscall.SIGTERM)

	grpclog.SetLoggerV2(grpclog.NewLoggerV2(ioutil.Discard, os.Stderr, os.Stderr))

	return &Service{
		Admin:           NewAdminServer(adminPort),
		signalsChannel:  signalsChannel,
		hcStatusChannel: hcStatusChannel,
	}
}

// AddFlags registers CLI flags.
func (s *Service) AddFlags(flagSet *flag.FlagSet) {
	AddConfigFileFlag(flagSet)
	if s.NoStorage {
		AddLoggingFlag(flagSet)
	} else {
		AddFlags(flagSet)
	}
	pMetrics.AddFlags(flagSet)
	s.Admin.AddFlags(flagSet)
}

// SetHealthCheckStatus sets status of healthcheck
func (s *Service) SetHealthCheckStatus(status healthcheck.Status) {
	s.hcStatusChannel <- healthcheck.Unavailable
}

// Start bootstraps the service and starts the admin server.
func (s *Service) Start(v *viper.Viper) error {
	if err := TryLoadConfigFile(v); err != nil {
		return errors.Wrap(err, "cannot load config file")
	}

	sFlags := new(SharedFlags).InitFromViper(v)
	newProdConfig := zap.NewProductionConfig()
	newProdConfig.Sampling = nil
	if logger, err := sFlags.NewLogger(newProdConfig); err == nil {
		s.Logger = logger
	} else {
		return errors.Wrap(err, "cannot create logger")
	}

	metricsBuilder := new(pMetrics.Builder).InitFromViper(v)
	metricsFactory, err := metricsBuilder.CreateMetricsFactory("")
	if err != nil {
		return errors.Wrap(err, "cannot create metrics factory")
	}
	s.MetricsFactory = metricsFactory

	s.Admin.initFromViper(v, s.Logger)
	if h := metricsBuilder.Handler(); h != nil {
		route := metricsBuilder.HTTPRoute
		s.Logger.Info("Mounting metrics handler on admin server", zap.String("route", route))
		s.Admin.Handle(route, h)
	}
	if err := s.Admin.Serve(); err != nil {
		return errors.Wrap(err, "cannot start the admin server")
	}

	return nil
}

// HC returns the reference to HeathCheck.
func (s *Service) HC() *healthcheck.HealthCheck {
	return s.Admin.HC()
}

// RunAndThen sets the health check to Ready and blocks until SIGTERM is received.
// If then runs the shutdown function and exits.
func (s *Service) RunAndThen(shutdown func()) {
	s.HC().Ready()

statusLoop:
	for {
		select {
		case status := <-s.hcStatusChannel:
			s.HC().Set(status)
		case <-s.signalsChannel:
			break statusLoop
		}
	}

	s.Logger.Info("Shutting down")
	s.HC().Set(healthcheck.Unavailable)

	if shutdown != nil {
		shutdown()
	}

	s.Admin.Close()
	s.Logger.Info("Shutdown complete")
}
