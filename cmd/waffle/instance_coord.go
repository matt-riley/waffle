package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/instance"
)

var makeServeOwnerCoordinator = func() (instance.Coordinator, error) {
	home, err := config.Home()
	if err != nil {
		return instance.Coordinator{}, err
	}
	return instance.Default(filepath.Join(home, "serve.lock")), nil
}

func acquireServeOwner(ctx context.Context) (*instance.Lease, error) {
	coordinator, err := makeServeOwnerCoordinator()
	if err != nil {
		return nil, err
	}
	lease, err := coordinator.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("waffle serve cannot start: %w", err)
	}
	return lease, nil
}

func refuseWorkspaceOpenWhileServing() error {
	coordinator, err := makeServeOwnerCoordinator()
	if err != nil {
		return err
	}
	record, err := coordinator.Check()
	if err != nil {
		return fmt.Errorf("check waffle serve owner: %w", err)
	}
	if record != nil {
		listen := "disabled"
		if path, pathErr := config.Path(); pathErr == nil {
			if cfg, loadErr := config.Load(path); loadErr == nil && cfg.Broker.Listen != "" {
				listen = cfg.Broker.Listen
			}
		}
		return fmt.Errorf(
			"waffle serve is running (pid %d) and owns broker listen address %s; `waffle ws open` cannot share its in-memory broker tokens; open the repo through the gateway (/repo) instead",
			record.PID, listen)
	}
	return nil
}
