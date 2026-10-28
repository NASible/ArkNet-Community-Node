// Package application defines the small contract used to manage local ArkNet applications.
package application

import (
	"context"
	"errors"
)

type ConfigurationStatus string

const (
	ConfigurationConfigured    ConfigurationStatus = "CONFIGURED"
	ConfigurationNotConfigured ConfigurationStatus = "NOT_CONFIGURED"
)

var (
	ErrDisabled             = errors.New("application is disabled")
	ErrLifecycleUnavailable = errors.New("application lifecycle control is unavailable")
)

// Adapter is implemented by each supported local application. It deliberately
// describes only the operations ArkNet needs and is not a plugin API.
type Adapter interface {
	ID() string
	Healthy(context.Context) bool
	Start(context.Context) error
	Stop(context.Context) error
	Restart(context.Context) error
	ConfigurationStatus() ConfigurationStatus
	Version(context.Context) (string, error)
	Enabled() bool
}

// Lifecycle controls the runtime hosting an application. Keeping it separate
// prevents adapters from depending directly on Docker or another orchestrator.
type Lifecycle interface {
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Restart(context.Context, string) error
}
