// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package console

import (
	"context"
	"math/rand"
	"time"

	"go.uber.org/zap"

	"storj.io/common/sync2"
	"storj.io/storj/satellite/metabase/segmentloop"
)

// Chore checks whether any emails need to be re-sent.
//
// architecture: Chore
type Chore struct {
	log    *zap.Logger
	Loop   *sync2.Cycle

	service *Service
	mailsender *mailservice.Sender
	config      Config
}

// NewChore instantiates Chore.
func NewChore(log *zap.Logger, service *Service, config Config) *Chore {
	return &Chore{
		log:    log,
		Loop:   sync2.NewCycle(config.ChoreInterval),

		service: service,
		config:      config,
	}
}

// Run starts the chore.
func (chore *Chore) Run(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)
	return chore.Loop.Run(ctx, func(ctx context.Context) (err error) {
		defer mon.Task()(&ctx)(&err)

		users, err:= chore.service.GetUsersNeedingEmailResend()

		for _, u:= range users{
			// do something like in sat/console/consolweb/ with SendAsync(
			chore.mailservice.SendEmail()
		}
	})
}

// Close closes chore.
func (chore *Chore) Close() error {
	chore.Loop.Close()
	return nil
}
