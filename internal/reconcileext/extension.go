/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package reconcileext

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/moby/extensions"
	servicev0 "github.com/moby/extensions/extpoints/service/v0"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	reconcilev0 "github.com/docker/compose/v5/internal/reconcileapi"
	reconcilepb "github.com/docker/compose/v5/internal/reconcileapi/protogen"
	"github.com/docker/compose/v5/internal/reconcileext/daemonapi"
	"github.com/docker/compose/v5/internal/reconcileext/servicegrpc"
)

// errStarting is what a call gets while the extension waits for the daemon's
// API: it maps to Unavailable.
var errStarting = errors.New("the reconciler is starting: the daemon does not serve its API yet")

// apiPollInterval is the delay between two probes of the daemon API at startup.
const apiPollInterval = 200 * time.Millisecond

// Extension is the reconcile extension.
//
// The daemon initializes its extensions before it serves its API, and the
// extension drives the daemon through that API: activation is therefore
// deferred. Init resolves the daemon.api point and returns; a goroutine waits
// for the daemon to answer a Ping, then publishes the reconciler. Calls landing
// before are answered Unavailable. (Same scheme as the daemon's jobs extension.)
type Extension struct {
	mu     sync.Mutex
	api    *client.Client
	active engine
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool

	// newEngine builds the engine on the daemon's API client; overridden by
	// tests
	newEngine func(client.APIClient) (engine, error)
}

// New returns the extension.
func New() *Extension {
	return &Extension{
		newEngine: func(c client.APIClient) (engine, error) { return newComposeEngine(c) },
	}
}

// Declaration describes the extension to the daemon: it publishes the reconcile
// gRPC service on the daemon's API socket, where the client reaches it, and
// depends on the daemon API point to act on the engine.
//
// The service is published through the service.grpc point (which the daemon
// proxies by service name) and not by offering the reconcile point itself: an
// executable extension's plain offer is recorded but never routed.
func (e *Extension) Declaration() extensions.Declaration {
	return extensions.Declaration{
		ID: reconcilev0.ExtensionID,
		Providers: []extensions.Provider{
			servicegrpc.Point.Provide(&publisher{svc: &service{ext: e}}),
			servicev0.Offer(servicegrpc.Point),
		},
		Dependencies: []extensions.Dependency{
			daemonapi.Point.Dependency(),
		},
		Init:     e.init,
		Shutdown: e.shutdown,
	}
}

func (e *Extension) init(ctx context.Context, _ extensions.Config, resolver extensions.Resolver) error {
	point, err := daemonapi.Point.Single(resolver)
	if err != nil {
		return fmt.Errorf("resolving the daemon API point: %w", err)
	}
	api, err := daemonapi.NewClient(point)
	if err != nil {
		return fmt.Errorf("creating the daemon API client: %w", err)
	}
	// the activation outlives Init: only shutdown ends it
	activateCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.mu.Lock()
	e.api = api
	e.cancel = cancel
	e.mu.Unlock()
	e.wg.Go(func() {
		if err := waitForAPI(activateCtx, api); err != nil {
			if activateCtx.Err() == nil {
				logrus.WithError(err).Error("reconcile: the daemon API never became ready; the reconciler stays unavailable")
			}
			return
		}
		eng, err := e.newEngine(api)
		if err != nil {
			logrus.WithError(err).Error("reconcile: activation failed; the reconciler stays unavailable")
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if !e.closed {
			e.active = eng
			logrus.Info("reconcile: active")
		}
	})
	return nil
}

func (e *Extension) shutdown(context.Context) error {
	e.mu.Lock()
	e.closed = true
	if e.cancel != nil {
		e.cancel()
	}
	e.mu.Unlock()
	e.wg.Wait()

	e.mu.Lock()
	api := e.api
	e.api, e.active = nil, nil
	e.mu.Unlock()
	if api != nil {
		return api.Close()
	}
	return nil
}

// current is the engine, or errStarting while there is none.
func (e *Extension) current() (engine, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active == nil {
		return nil, errStarting
	}
	return e.active, nil
}

// waitForAPI blocks until the daemon serves API requests: it answers 503
// until its start completes.
func waitForAPI(ctx context.Context, api *client.Client) error {
	for {
		if _, err := api.Ping(ctx, client.PingOptions{}); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(apiPollInterval):
		}
	}
}

// service is the reconcile point as the daemon's gRPC server sees it: it
// answers Unavailable until the extension is active and gives every error its
// gRPC code.
type service struct {
	ext *Extension
}

var _ reconcilev0.Reconciler = (*service)(nil)

func (s *service) reconciler() (*reconciler, error) {
	eng, err := s.ext.current()
	if err != nil {
		return nil, statusError(err)
	}
	return &reconciler{engine: eng}, nil
}

func (s *service) Apply(ctx context.Context, req *reconcilev0.ApplyRequest) (*reconcilev0.ApplyReply, error) {
	r, err := s.reconciler()
	if err != nil {
		return nil, err
	}
	reply, err := r.Apply(ctx, req)
	return reply, statusError(err)
}

func (s *service) Remove(ctx context.Context, req *reconcilev0.RemoveRequest) (*reconcilev0.RemoveReply, error) {
	r, err := s.reconciler()
	if err != nil {
		return nil, err
	}
	reply, err := r.Remove(ctx, req)
	return reply, statusError(err)
}

func (s *service) Admit(ctx context.Context, req *reconcilev0.AdmitRequest) (*reconcilev0.AdmitReply, error) {
	// admitting touches nothing: it needs no engine, so it answers while the
	// extension is still starting
	reply, err := (&reconciler{}).Admit(ctx, req)
	return reply, statusError(err)
}

// publisher registers the reconcile service for publication on docker.sock.
type publisher struct {
	svc *service
}

var _ servicegrpc.Provider = (*publisher)(nil)

func (p *publisher) RegisterServices(r grpc.ServiceRegistrar) {
	reconcilepb.ServerPoint.Register(r, p.svc)
}
