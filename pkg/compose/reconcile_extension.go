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

package compose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	extensionclient "github.com/moby/extensions/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/docker/compose/v5/internal/progress"
	reconcilev0 "github.com/docker/compose/v5/internal/reconcileapi"
	reconcilepb "github.com/docker/compose/v5/internal/reconcileapi/protogen"
	"github.com/docker/compose/v5/pkg/api"
)

// EXPERIMENTAL: reconciliation delegated to the daemon.
//
// With WithReconcileExtension (the CLI turns it on when the environment
// variable COMPOSE_EXPERIMENTAL_RECONCILE_EXTENSION is truthy), `up` and `down`
// no longer reconcile the daemon's resources themselves: they hand the model
// to the daemon's Compose reconcile extension (internal/reconcileapi) and
// replay the plan it returns as progress events. The client keeps what only it
// can do: building images, pulling them with the credentials of the CLI, and
// reading the files the model refers to (secrets and configs are inlined).
//
// Off, nothing of this file runs.

// daemonReconciler is the part of the daemon's reconcile extension the client drives.
// It is the extension point's own interface, named here so that the flow can
// be tested against a double.
type daemonReconciler = reconcilev0.Reconciler

// WithReconcileExtension makes Up and Down delegate the reconciliation to the
// daemon's Compose reconcile extension. EXPERIMENTAL.
func WithReconcileExtension(s *composeService) error {
	s.reconcileExt.enabled = true
	return nil
}

// reconcileExtState is the lazily resolved connection to the extension. As for
// the jobs extension, resolving it dials nothing: an engine without the
// extension only reports codes.Unimplemented on the first call.
type reconcileExtState struct {
	enabled bool

	once   sync.Once
	client *extensionclient.Client
	api    daemonReconciler
	err    error
}

func (s *composeService) reconcileExtension() (daemonReconciler, error) {
	s.reconcileExt.once.Do(func() {
		exts, err := extensionclient.New(s.apiClient(), extensionclient.WithGRPCPoint(reconcilepb.ClientPoint))
		if err != nil {
			s.reconcileExt.err = err
			return
		}
		s.reconcileExt.client = exts
		s.reconcileExt.api, s.reconcileExt.err = extensionclient.Resolve(exts, reconcilev0.Point)
	})
	return s.reconcileExt.api, s.reconcileExt.err
}

const unsupportedWithReconcileExtension = "not supported with the reconcile extension (experimental)"

// rejectUnsupportedByReconcileExtension refuses what the extension cannot
// carry out, before anything is built or pulled.
func rejectUnsupportedByReconcileExtension(project *types.Project, options api.UpOptions) error {
	var unsupported []string
	if options.Start.Attach != nil {
		unsupported = append(unsupported, "up without --detach")
	}
	if options.Start.Watch {
		unsupported = append(unsupported, "--watch")
	}
	if options.Start.OnExit != api.CascadeIgnore {
		unsupported = append(unsupported, "--abort-on-container-exit and --abort-on-container-failure")
	}
	if options.Start.ExitCodeFrom != "" {
		unsupported = append(unsupported, "--exit-code-from")
	}
	if len(project.Models) > 0 {
		unsupported = append(unsupported, "models")
	}
	for name, service := range project.Services {
		if service.Provider != nil {
			unsupported = append(unsupported, fmt.Sprintf("provider services (%s)", name))
		}
		if service.UseAPISocket {
			unsupported = append(unsupported, fmt.Sprintf("use_api_socket (%s)", name))
		}
	}
	for name, job := range project.Jobs {
		if HasSchedule(job) {
			unsupported = append(unsupported, fmt.Sprintf("scheduled jobs (%s)", name))
		}
	}
	if len(unsupported) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %s", strings.Join(unsupported, ", "), unsupportedWithReconcileExtension)
}

// upViaExtension is Up when the reconciliation is delegated to the daemon.
func (s *composeService) upViaExtension(ctx context.Context, project *types.Project, options api.UpOptions) error {
	if err := rejectUnsupportedByReconcileExtension(project, options); err != nil {
		return err
	}
	rec, err := s.reconcileExtension()
	if err != nil {
		return err
	}
	return Run(ctx, func(ctx context.Context) error {
		model, err := s.prepareReconcileModel(ctx, project, options.Create)
		if err != nil {
			return err
		}
		reply, err := applyReconcile(ctx, rec, applyRequest(project.Name, model, options, s.dryRun))
		if err != nil {
			return err
		}
		s.replaySteps(reply.Steps)
		return nil
	}, "up", s.events)
}

// downViaExtension is Down when the reconciliation is delegated to the daemon.
func (s *composeService) downViaExtension(ctx context.Context, projectName string, options api.DownOptions) error {
	var unsupported []string
	if options.Images != api.ImagePruneNone {
		unsupported = append(unsupported, "--rmi")
	}
	if len(options.Services) > 0 {
		unsupported = append(unsupported, "down of selected services")
	}
	if s.dryRun {
		unsupported = append(unsupported, "--dry-run")
	}
	if len(unsupported) > 0 {
		return fmt.Errorf("%s: %s", strings.Join(unsupported, ", "), unsupportedWithReconcileExtension)
	}
	rec, err := s.reconcileExtension()
	if err != nil {
		return err
	}
	return Run(ctx, func(ctx context.Context) error {
		reply, err := rec.Remove(ctx, removeRequest(strings.ToLower(projectName), options))
		if err != nil {
			return translateReconcileError(err)
		}
		s.replaySteps(reply.Steps)
		return nil
	}, "down", s.events)
}

func removeRequest(projectName string, options api.DownOptions) *reconcilev0.RemoveRequest {
	req := &reconcilev0.RemoveRequest{
		Project:       projectName,
		Volumes:       options.Volumes,
		RemoveOrphans: options.RemoveOrphans,
	}
	if options.Timeout != nil {
		req.TimeoutSeconds = ceilSeconds(*options.Timeout)
	}
	return req
}

func applyRequest(projectName string, model []byte, options api.UpOptions, dryRun bool) *reconcilev0.ApplyRequest {
	req := &reconcilev0.ApplyRequest{
		Project:       projectName,
		Model:         model,
		Services:      options.Create.Services,
		Recreate:      reconcileRecreate(options.Create.Recreate),
		RemoveOrphans: options.Create.RemoveOrphans,
		Wait:          options.Start.Wait,
		DryRun:        dryRun,
	}
	if options.Start.Wait {
		req.WaitTimeoutSeconds = ceilSeconds(options.Start.WaitTimeout)
	}
	return req
}

func reconcileRecreate(policy string) string {
	switch policy {
	case api.RecreateForce:
		return reconcilev0.RecreateForce
	case api.RecreateNever:
		return reconcilev0.RecreateNever
	default:
		return reconcilev0.RecreateDiverged
	}
}

// ceilSeconds is d in whole seconds, rounded up: the contract counts seconds,
// and zero means "no bound", which a sub-second timeout must not become.
func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + time.Second - 1) / time.Second)
}

// applyReconcile asks the extension whether it admits the model, then applies it.
func applyReconcile(ctx context.Context, rec daemonReconciler, req *reconcilev0.ApplyRequest) (*reconcilev0.ApplyReply, error) {
	admitted, err := rec.Admit(ctx, &reconcilev0.AdmitRequest{Project: req.Project, Model: req.Model})
	if err != nil {
		return nil, translateReconcileError(err)
	}
	if len(admitted.Refusals) > 0 {
		var msg strings.Builder
		msg.WriteString("the daemon's reconcile extension cannot apply this model:")
		for _, refusal := range admitted.Refusals {
			fmt.Fprintf(&msg, "\n - %s: %s", refusal.Path, refusal.Reason)
		}
		return nil, errors.New(msg.String())
	}
	reply, err := rec.Apply(ctx, req)
	if err != nil {
		return nil, translateReconcileError(err)
	}
	return reply, nil
}

// translateReconcileError turns the gRPC status of the extension into an error
// the user can act on (see the error contract of reconcilev0.Reconciler).
func translateReconcileError(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	switch st.Code() {
	case codes.Unimplemented:
		return fmt.Errorf("the daemon does not provide the Compose reconcile extension (%s): %s", reconcilev0.Point.ID(), st.Message())
	case codes.Unavailable:
		return fmt.Errorf("the daemon's Compose reconcile extension is not ready: %s", st.Message())
	case codes.InvalidArgument:
		return errors.New(st.Message())
	case codes.FailedPrecondition:
		return fmt.Errorf("the reconciliation failed halfway, the daemon is left as far as it went: %s", st.Message())
	case codes.DeadlineExceeded:
		return fmt.Errorf("timeout waiting for the services: %s", st.Message())
	case codes.Canceled:
		return context.Canceled
	default:
		return err
	}
}

// prepareReconcileModel does what only the client can do for the model to be
// self-contained, then serializes it the way `docker compose config` does:
//   - images are built, and pulled when the pull policy requires it, through
//     the daemon's API with the credentials of the CLI. The daemon is then told
//     (pull_policy: never) not to pull again: it has no credentials, and an
//     "always" policy would otherwise pull a second time. Pulling client-side
//     is right whichever daemon is targeted, since the images land in it;
//   - the sections build are dropped (the images exist), after the image name
//     the build produced is made explicit;
//   - the files of secrets and configs are read and inlined as content.
func (s *composeService) prepareReconcileModel(ctx context.Context, project *types.Project, options api.CreateOptions) ([]byte, error) {
	project = project.WithoutUnresolvedOptionalDependencies()
	if err := project.CheckContainerNameUnicity(); err != nil {
		return nil, err
	}
	if err := s.ensureImagesExists(ctx, project, options.Build, options.QuietPull, true); err != nil {
		return nil, err
	}
	return canonicalReconcileModel(project)
}

// canonicalReconcileModel is the document sent to the extension.
func canonicalReconcileModel(project *types.Project) ([]byte, error) {
	project, err := project.WithServicesTransform(func(name string, service types.ServiceConfig) (types.ServiceConfig, error) {
		if service.Image == "" && service.Build != nil {
			service.Image = api.GetImageNameOrDefault(service, project.Name)
		}
		service.Build = nil
		service.PullPolicy = types.PullPolicyNever
		return service, nil
	})
	if err != nil {
		return nil, err
	}
	project, err = project.WithServicesEnvironmentResolved(true)
	if err != nil {
		return nil, err
	}
	if err := inlineFileObjects(project); err != nil {
		return nil, err
	}
	content, err := project.MarshalYAML(types.WithSecretContent)
	if err != nil {
		return nil, err
	}
	// same escaping as `docker compose config`: the document is a Compose file
	// the extension reads back, interpolation included.
	return bytes.ReplaceAll(content, []byte{'$'}, []byte{'$', '$'}), nil
}

// inlineFileObjects replaces the file and environment sources of secrets and
// configs by their content: the daemon has neither the client's files nor its
// environment. The project is modified in place, it has to be a copy.
func inlineFileObjects(project *types.Project) error {
	for name, secret := range project.Secrets {
		inlined, err := inlineFileObject(project, types.FileObjectConfig(secret))
		if err != nil {
			return fmt.Errorf("secret %q: %w", name, err)
		}
		project.Secrets[name] = types.SecretConfig(inlined)
	}
	for name, config := range project.Configs {
		inlined, err := inlineFileObject(project, types.FileObjectConfig(config))
		if err != nil {
			return fmt.Errorf("config %q: %w", name, err)
		}
		project.Configs[name] = types.ConfigObjConfig(inlined)
	}
	return nil
}

func inlineFileObject(project *types.Project, obj types.FileObjectConfig) (types.FileObjectConfig, error) {
	if obj.External {
		return obj, nil
	}
	switch {
	case obj.File != "":
		content, err := os.ReadFile(obj.File)
		if err != nil {
			return obj, err
		}
		obj.Content, obj.File = string(content), ""
	case obj.Environment != "":
		if obj.Content == "" {
			value, ok := project.Environment[obj.Environment]
			if !ok {
				return obj, fmt.Errorf("environment variable %q is not set", obj.Environment)
			}
			obj.Content = value
		}
		obj.Environment = ""
	}
	return obj, nil
}

// replaySteps reports the operations the extension performed (or would, with
// a dry run) as the progress events the client's own engine emits, so that the
// output looks like the one of a local reconciliation. The extension answers
// once everything is done, so each operation is reported as started then done.
func (s *composeService) replaySteps(steps []reconcilev0.Step) {
	for _, step := range steps {
		working, done, ok := stepEvents(step)
		if !ok {
			continue
		}
		s.events.On(working)
		s.events.On(done)
	}
}

// stepEvents maps a step to its working and done events. Operations with no
// counterpart in the progress display (connections, renames, waits) have none.
func stepEvents(step reconcilev0.Step) (working, done api.Resource, ok bool) {
	var kind string
	switch step.Operation {
	case "create_network", "remove_network":
		kind = "Network "
	case "create_volume", "remove_volume":
		kind = "Volume "
	case "create_container", "start_container", "stop_container", "remove_container":
		kind = "Container "
	default:
		return working, done, false
	}
	id := kind + step.Resource
	switch step.Operation {
	case "create_network", "create_volume", "create_container":
		return progress.Creating(id), progress.Created(id), true
	case "start_container":
		return progress.New(id, api.Working, api.StatusStarting), progress.New(id, api.Done, api.StatusStarted), true
	case "stop_container":
		return progress.Stopping(id), progress.Stopped(id), true
	default:
		return progress.Removing(id), progress.Removed(id), true
	}
}
