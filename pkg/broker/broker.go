// Copyright 2022 TriggerMesh Inc.
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/triggermesh/brokers/pkg/backend"
	"github.com/triggermesh/brokers/pkg/broker/cmd"
	"github.com/triggermesh/brokers/pkg/common/fs"
	cfgbwatcher "github.com/triggermesh/brokers/pkg/config/broker/watcher"
	cfgowatcher "github.com/triggermesh/brokers/pkg/config/observability/watcher"
	"github.com/triggermesh/brokers/pkg/ingest"
	"github.com/triggermesh/brokers/pkg/subscriptions"
)

type Status string

const (
	StatusStopped  Status = "stopped"
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusStopping Status = "stopping"
)

type Instance struct {
	backend      backend.Interface
	ingest       *ingest.Instance
	subscription *subscriptions.Manager
	cw           *cfgbwatcher.Watcher
	ocw          *cfgowatcher.Watcher
	status       Status

	logger *zap.SugaredLogger
}

func NewInstance(globals *cmd.Globals, b backend.Interface) (*Instance, error) {
	globals.Logger.Debug("Creating subscription manager")
	sm, err := subscriptions.New(globals.Logger.Named("subs"), b)
	if err != nil {
		return nil, err
	}

	// The ConfigWatcher will read the configfile and call registered
	// callbacks upon start and everytime the configuration file
	// is updated.
	cfw, err := fs.NewCachedFileWatcher(globals.Logger.Named("fswatch"))
	if err != nil {
		return nil, err
	}

	configPath, err := filepath.Abs(globals.BrokerConfigPath)
	if err != nil {
		return nil, fmt.Errorf("error resolving to absolute path %q: %w", globals.BrokerConfigPath, err)
	}

	globals.Logger.Debugw("Creating watcher for broker configuration", zap.String("file", configPath))
	cfgw, err := cfgbwatcher.NewWatcher(cfw, configPath, globals.Logger.Named("cgfwatch"))
	if err != nil {
		return nil, err
	}

	var ocfgw *cfgowatcher.Watcher
	if globals.ObservabilityConfigPath != "" {
		obsCfgPath, err := filepath.Abs(globals.ObservabilityConfigPath)
		if err != nil {
			return nil, fmt.Errorf("error resolving to absolute path %q: %w", globals.ObservabilityConfigPath, err)
		}

		globals.Logger.Debugw("Creating watcher for observability configuration", zap.String("file", obsCfgPath))
		ocfgw, err = cfgowatcher.NewWatcher(cfw, obsCfgPath, globals.Logger.Named("ocgfwatch"))
		if err != nil {
			return nil, err
		}

		ocfgw.AddCallback(globals.UpdateLevel)
	}

	globals.Logger.Debug("Creating HTTP ingest server")
	i := ingest.NewInstance(globals.Logger.Named("ingest"),
		ingest.InstanceWithPort(globals.Port),
	)

	globals.Logger.Debug("Creating broker instance")
	return &Instance{
		backend:      b,
		ingest:       i,
		subscription: sm,
		cw:           cfgw,
		ocw:          ocfgw,
		status:       StatusStopped,

		logger: globals.Logger.Named("broker"),
	}, nil
}

func (i *Instance) Start(inctx context.Context) error {
	i.logger.Debug("Starting broker instance")
	i.status = StatusStarting

	sigctx, stop := signal.NotifyContext(inctx, os.Interrupt, syscall.SIGTERM)
	defer func() {
		stop()
		i.status = StatusStopped
	}()

	grp, ctx := errgroup.WithContext(sigctx)
	go func() {
		<-ctx.Done()
		// In case we receive the context done signal but the
		// status was already set to Stopped.
		if i.status != StatusStopped {
			i.status = StatusStopping
		}
	}()

	// Initialization will create structures, execute migrations
	// and claim non processed messages from the backend.
	i.logger.Debug("Initializing backend")
	err := i.backend.Init(ctx)
	if err != nil {
		return fmt.Errorf("could not initialize backend: %w", err)
	}

	// Start is a blocking function that will read messages from the backend
	// implementation and send them to the subscription manager dispatcher.
	// When the dispatcher returns the message is marked as processed.
	i.logger.Debug("Starting backend routine")
	grp.Go(func() error {
		return i.backend.Start(ctx)
	})

	// ConfigWatcher will callback reconfigurations for:
	// - Ingest: if authentication parameters are updated.
	// - Subscription manager: if triggers configurations changes.
	i.logger.Debug("Adding config watcher callbacks")
	i.cw.AddCallback(i.ingest.UpdateFromConfig)
	i.cw.AddCallback(i.subscription.UpdateFromConfig)

	// Start the configuration watcher for brokers.
	// There is no need to add it to the wait group
	// since it cleanly exits when context is done.
	i.logger.Debug("Starting broker configuration watcher")
	if err = i.cw.Start(ctx); err != nil {
		return fmt.Errorf("could not start broker configuration watcher: %w", err)
	}

	// Start the configuration watcher for observability.
	if i.ocw != nil {
		i.logger.Debug("Starting observability configuration watcher")
		if err = i.ocw.Start(ctx); err != nil {
			return fmt.Errorf("could not start observability configuration watcher: %w", err)
		}
	}

	// Register producer function for received events at ingest.
	i.ingest.RegisterCloudEventHandler(i.backend.Produce)

	// TODO register probes at ingest

	// Start the server that ingests CloudEvents.
	grp.Go(func() error {
		err := i.ingest.Start(ctx)
		return err
	})

	i.status = StatusRunning

	return grp.Wait()
}

func (i *Instance) GetStatus() Status {
	return i.status
}
