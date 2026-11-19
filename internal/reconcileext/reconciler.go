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
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	reconcilev0 "github.com/docker/compose/v5/internal/reconcileapi"
	"github.com/docker/compose/v5/pkg/api"
)

// reconciler implements the reconcile point on an [engine].
//
// Its errors are plain Go errors; [statusError] gives them their gRPC code at
// the boundary.
type reconciler struct {
	engine engine
}

var _ reconcilev0.Reconciler = (*reconciler)(nil)

func (r *reconciler) Admit(ctx context.Context, req *reconcilev0.AdmitRequest) (*reconcilev0.AdmitReply, error) {
	project, err := loadModel(ctx, req.Project, req.Model)
	if err != nil {
		return nil, err
	}
	return &reconcilev0.AdmitReply{Refusals: admit(project)}, nil
}

func (r *reconciler) Apply(ctx context.Context, req *reconcilev0.ApplyRequest) (*reconcilev0.ApplyReply, error) {
	project, err := loadModel(ctx, req.Project, req.Model)
	if err != nil {
		return nil, err
	}
	if refusals := admit(project); len(refusals) > 0 {
		return nil, refusalsError(refusals)
	}
	labelServices(project, req.WorkingDir, req.ConfigFiles)
	options, err := upOptions(req)
	if err != nil {
		return nil, err
	}
	// the services to converge, with what they depend on: "narrow the project"
	// is how Compose restricts a convergence
	if len(req.Services) > 0 {
		project, err = project.WithSelectedServices(req.Services)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errModel, err)
		}
	}

	if req.DryRun {
		steps, err := r.engine.Plan(ctx, project, options)
		if err != nil {
			return nil, err
		}
		return &reconcilev0.ApplyReply{Steps: steps}, nil
	}

	steps, err := r.engine.Up(ctx, project, options)
	if err != nil {
		return nil, err
	}
	// the state is read on a context of its own: the work is done, a caller
	// leaving now must not turn it into a failure of the report
	services, err := r.engine.Services(context.WithoutCancel(ctx), project)
	if err != nil {
		return nil, err
	}
	return &reconcilev0.ApplyReply{Steps: steps, Services: services}, nil
}

func (r *reconciler) Remove(ctx context.Context, req *reconcilev0.RemoveRequest) (*reconcilev0.RemoveReply, error) {
	if req.Project == "" {
		return nil, fmt.Errorf("%w: project name is required", errModel)
	}
	options := api.DownOptions{
		// the containers no model accounts for are removed with the project
		RemoveOrphans: true,
		Volumes:       req.Volumes,
	}
	if req.TimeoutSeconds > 0 {
		timeout := time.Duration(req.TimeoutSeconds) * time.Second
		options.Timeout = &timeout
	}
	steps, err := r.engine.Down(ctx, req.Project, options)
	if err != nil {
		return nil, err
	}
	return &reconcilev0.RemoveReply{Steps: steps}, nil
}

// upOptions turns the request into the options of a detached `up`.
func upOptions(req *reconcilev0.ApplyRequest) (api.UpOptions, error) {
	var recreate string
	switch req.Recreate {
	case "", reconcilev0.RecreateDiverged:
		recreate = api.RecreateDiverged
	case reconcilev0.RecreateNever:
		recreate = api.RecreateNever
	case reconcilev0.RecreateForce:
		recreate = api.RecreateForce
	default:
		return api.UpOptions{}, fmt.Errorf("%w: unknown recreate policy %q (one of %s, %s, %s)", errModel,
			req.Recreate, reconcilev0.RecreateDiverged, reconcilev0.RecreateNever, reconcilev0.RecreateForce)
	}
	if req.WaitTimeoutSeconds < 0 {
		return api.UpOptions{}, fmt.Errorf("%w: negative wait timeout", errModel)
	}
	return api.UpOptions{
		Create: api.CreateOptions{
			Services:             req.Services,
			Recreate:             recreate,
			RecreateDependencies: recreate,
			RemoveOrphans:        req.RemoveOrphans,
			IgnoreOrphans:        !req.RemoveOrphans,
			// what the CLI does by default: keep anonymous volumes of a
			// container that gets recreated
			Inherit:   true,
			QuietPull: true,
		},
		Start: api.StartOptions{
			Wait:        req.Wait,
			WaitTimeout: time.Duration(req.WaitTimeoutSeconds) * time.Second,
		},
	}, nil
}

// statusError gives an error of the reconciler its gRPC code, as the contract
// documents:
//
//	InvalidArgument    the model cannot be read or is refused
//	Canceled           the caller went away
//	DeadlineExceeded   the caller's deadline passed
//	Unavailable        the extension is starting (see [errStarting])
//	FailedPrecondition the convergence failed, the daemon is left as far as it went
//
// An error already carrying a status is left as is.
func statusError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, errModel):
		return status.Error(codes.InvalidArgument, strings.TrimPrefix(err.Error(), errModel.Error()+": "))
	case errors.Is(err, errStarting):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	default:
		return status.Error(codes.FailedPrecondition, err.Error())
	}
}
