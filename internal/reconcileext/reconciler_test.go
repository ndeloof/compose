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
	"net"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"gotest.tools/v3/assert"

	"github.com/docker/compose/v5/internal/planning"
	reconcilev0 "github.com/docker/compose/v5/internal/reconcileapi"
	reconcilepb "github.com/docker/compose/v5/internal/reconcileapi/protogen"
	"github.com/docker/compose/v5/pkg/api"
)

// fakeEngine records what the reconciler asks and answers from its fields.
type fakeEngine struct {
	upProject   *types.Project
	upOptions   api.UpOptions
	planned     bool
	downProject string
	downOptions api.DownOptions

	steps    []reconcilev0.Step
	services []reconcilev0.ServiceState
	err      error
}

func (f *fakeEngine) Up(_ context.Context, p *types.Project, o api.UpOptions) ([]reconcilev0.Step, error) {
	f.upProject, f.upOptions = p, o
	return f.steps, f.err
}

func (f *fakeEngine) Plan(_ context.Context, p *types.Project, o api.UpOptions) ([]reconcilev0.Step, error) {
	f.planned = true
	f.upProject, f.upOptions = p, o
	return f.steps, f.err
}

func (f *fakeEngine) Services(context.Context, *types.Project) ([]reconcilev0.ServiceState, error) {
	return f.services, nil
}

func (f *fakeEngine) Down(_ context.Context, name string, o api.DownOptions) ([]reconcilev0.Step, error) {
	f.downProject, f.downOptions = name, o
	return f.steps, f.err
}

func TestApplyOptions(t *testing.T) {
	fake := &fakeEngine{steps: []reconcilev0.Step{{Operation: "create_container", Resource: "demo-web-1", Service: "web"}}}
	r := &reconciler{engine: fake}

	reply, err := r.Apply(t.Context(), &reconcilev0.ApplyRequest{
		Project:            "demo",
		Model:              []byte(okModel),
		WorkingDir:         "/home/me/app",
		ConfigFiles:        []string{"/home/me/app/compose.yaml"},
		Services:           []string{"web"},
		Recreate:           reconcilev0.RecreateForce,
		RemoveOrphans:      true,
		Wait:               true,
		WaitTimeoutSeconds: 30,
	})
	assert.NilError(t, err)
	assert.Equal(t, len(reply.Steps), 1)
	assert.Assert(t, !fake.planned)

	assert.DeepEqual(t, fake.upProject.ServiceNames(), []string{"db", "web"}) // web depends on db
	assert.Equal(t, fake.upProject.Services["web"].CustomLabels[api.WorkingDirLabel], "/home/me/app")
	assert.Equal(t, fake.upProject.Services["web"].CustomLabels[api.ProjectLabel], "demo")
	assert.Equal(t, fake.upOptions.Create.Recreate, api.RecreateForce)
	assert.Assert(t, fake.upOptions.Create.RemoveOrphans)
	assert.Assert(t, !fake.upOptions.Create.IgnoreOrphans)
	assert.Assert(t, fake.upOptions.Create.Inherit)
	assert.Assert(t, fake.upOptions.Start.Wait)
	assert.Equal(t, fake.upOptions.Start.WaitTimeout, 30*time.Second)
	assert.Assert(t, fake.upOptions.Start.Attach == nil, "detached")
}

func TestApplyDefaultsAndDryRun(t *testing.T) {
	fake := &fakeEngine{}
	r := &reconciler{engine: fake}
	_, err := r.Apply(t.Context(), &reconcilev0.ApplyRequest{Project: "demo", Model: []byte(okModel), DryRun: true})
	assert.NilError(t, err)
	assert.Assert(t, fake.planned, "a dry run only plans")
	assert.Equal(t, fake.upOptions.Create.Recreate, api.RecreateDiverged)
	assert.Assert(t, fake.upOptions.Create.IgnoreOrphans)
	assert.DeepEqual(t, fake.upProject.ServiceNames(), []string{"db", "web"})
}

func TestApplyRefusesBeforeTouchingAnything(t *testing.T) {
	fake := &fakeEngine{}
	r := &reconciler{engine: fake}
	_, err := r.Apply(t.Context(), &reconcilev0.ApplyRequest{Project: "demo", Model: []byte("services:\n  a:\n    build: .\n")})
	assert.Assert(t, errors.Is(err, errModel))
	assert.ErrorContains(t, err, "services.a.build")
	assert.Assert(t, fake.upProject == nil)

	_, err = r.Apply(t.Context(), &reconcilev0.ApplyRequest{Project: "demo", Model: []byte(okModel), Recreate: "sometimes"})
	assert.Assert(t, errors.Is(err, errModel))
	_, err = r.Apply(t.Context(), &reconcilev0.ApplyRequest{Project: "demo", Model: []byte(okModel), Services: []string{"ghost"}})
	assert.Assert(t, errors.Is(err, errModel), "%v", err)
	assert.Assert(t, fake.upProject == nil)
}

func TestRemove(t *testing.T) {
	fake := &fakeEngine{steps: []reconcilev0.Step{{Operation: "remove_container", Resource: "demo-web-1"}}}
	r := &reconciler{engine: fake}
	reply, err := r.Remove(t.Context(), &reconcilev0.RemoveRequest{Project: "demo", Volumes: true, TimeoutSeconds: 5})
	assert.NilError(t, err)
	assert.Equal(t, len(reply.Steps), 1)
	assert.Equal(t, fake.downProject, "demo")
	assert.Assert(t, fake.downOptions.Volumes && fake.downOptions.RemoveOrphans)
	assert.Equal(t, *fake.downOptions.Timeout, 5*time.Second)

	_, err = r.Remove(t.Context(), &reconcilev0.RemoveRequest{})
	assert.Assert(t, errors.Is(err, errModel))
}

func TestStatusError(t *testing.T) {
	for name, tt := range map[string]struct {
		err  error
		code codes.Code
	}{
		"refused model":  {refusalsError([]reconcilev0.Refusal{{Path: "p", Reason: "r"}}), codes.InvalidArgument},
		"unreadable":     {fmt.Errorf("%w: %w", errModel, errors.New("yaml")), codes.InvalidArgument},
		"starting":       {errStarting, codes.Unavailable},
		"caller left":    {fmt.Errorf("pulling: %w", context.Canceled), codes.Canceled},
		"caller timeout": {context.DeadlineExceeded, codes.DeadlineExceeded},
		"half done":      {errors.New("create container: no space left"), codes.FailedPrecondition},
		"already status": {status.Error(codes.NotFound, "x"), codes.NotFound},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, status.Code(statusError(tt.err)), tt.code)
		})
	}
	assert.NilError(t, statusError(nil))
	assert.Equal(t, status.Convert(statusError(refusalsError([]reconcilev0.Refusal{{Path: "models.m", Reason: "no"}}))).Message(), "models.m: no")
}

func TestStepsOfNamesResources(t *testing.T) {
	plan := &planning.Plan{}
	network := plan.AddNode(planning.Operation{Type: planning.OpCreateNetwork, ResourceID: "network:default", Name: "demo_default", Cause: "not found"}, "")
	vol := plan.AddNode(planning.Operation{Type: planning.OpCreateVolume, ResourceID: "volume:data", Name: "demo_data"}, "")
	web := &types.ServiceConfig{Name: "web"}
	// a replacement: created under a temporary name, renamed once the old one is gone
	create := plan.AddNode(planning.Operation{Type: planning.OpCreateContainer, ResourceID: "service:web:1", Service: web, Name: "tmp_demo-web-1"}, "r", network, vol)
	plan.AddNode(planning.Operation{Type: planning.OpRenameContainer, ResourceID: "service:web:1", Service: web, Name: "demo-web-1", CreateNodeID: create.ID}, "r", create)
	plan.AddNode(planning.Operation{Type: planning.OpStartContainer, ResourceID: "service:web:1", Service: web, CreateNodeID: create.ID}, "", create)

	steps := stepsOf(plan)
	assert.DeepEqual(t, steps, []reconcilev0.Step{
		{Operation: "create_network", Resource: "demo_default", Reason: "not found"},
		{Operation: "create_volume", Resource: "demo_data"},
		{Operation: "create_container", Resource: "demo-web-1", Service: "web"},
		{Operation: "rename_container", Resource: "demo-web-1", Service: "web"},
		{Operation: "start_container", Resource: "demo-web-1", Service: "web"},
	})
}

func TestServiceOf(t *testing.T) {
	assert.Equal(t, serviceOf(planning.Operation{ResourceID: "service:web:2"}), "web")
	assert.Equal(t, serviceOf(planning.Operation{ResourceID: "network:default"}), "")
}

// grpcClient serves service over an in-memory connection, exactly as the
// daemon's SDK server registers it, and returns the generated client.
func grpcClient(t *testing.T, svc reconcilev0.Reconciler) reconcilepb.ReconcilerClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	reconcilepb.ServerPoint.Register(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	assert.NilError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return reconcilepb.NewReconcilerClient(conn)
}

func TestGRPCEndToEndWithAFakeEngine(t *testing.T) {
	fake := &fakeEngine{
		steps:    []reconcilev0.Step{{Operation: "create_container", Resource: "demo-web-1", Service: "web"}},
		services: []reconcilev0.ServiceState{{Name: "web", Containers: []string{"abc"}, Running: 1}},
	}
	ext := New()
	ext.setActive(fake)
	c := grpcClient(t, &service{ext: ext})

	admitted, err := c.Admit(t.Context(), &reconcilepb.AdmitRequest{Project: "demo", Model: []byte(okModel)})
	assert.NilError(t, err)
	assert.Equal(t, len(admitted.Refusals), 0)

	refused, err := c.Admit(t.Context(), &reconcilepb.AdmitRequest{Project: "demo", Model: []byte("services:\n  a:\n    build: .\n")})
	assert.NilError(t, err)
	assert.Equal(t, len(refused.Refusals), 1)
	assert.Equal(t, refused.Refusals[0].Path, "services.a.build")

	reply, err := c.Apply(t.Context(), &reconcilepb.ApplyRequest{Project: "demo", Model: []byte(okModel)})
	assert.NilError(t, err)
	assert.Equal(t, reply.Steps[0].Resource, "demo-web-1")
	assert.Equal(t, reply.Services[0].Running, uint32(1))

	_, err = c.Apply(t.Context(), &reconcilepb.ApplyRequest{Project: "demo", Model: []byte("services:\n  a:\n    build: .\n")})
	assert.Equal(t, status.Code(err), codes.InvalidArgument)
	assert.ErrorContains(t, err, "services.a.build")

	fake.err = errors.New("engine failure")
	_, err = c.Apply(t.Context(), &reconcilepb.ApplyRequest{Project: "demo", Model: []byte(okModel)})
	assert.Equal(t, status.Code(err), codes.FailedPrecondition)

	fake.err = nil
	removed, err := c.Remove(t.Context(), &reconcilepb.RemoveRequest{Project: "demo"})
	assert.NilError(t, err)
	assert.Equal(t, len(removed.Steps), 1)
}

func TestUnavailableUntilActive(t *testing.T) {
	ext := New()
	c := grpcClient(t, &service{ext: ext})

	_, err := c.Apply(t.Context(), &reconcilepb.ApplyRequest{Project: "demo", Model: []byte(okModel)})
	assert.Equal(t, status.Code(err), codes.Unavailable)
	_, err = c.Remove(t.Context(), &reconcilepb.RemoveRequest{Project: "demo"})
	assert.Equal(t, status.Code(err), codes.Unavailable)
	// admitting touches nothing and needs no engine
	_, err = c.Admit(t.Context(), &reconcilepb.AdmitRequest{Project: "demo", Model: []byte(okModel)})
	assert.NilError(t, err)

	ext.setActive(&fakeEngine{})
	_, err = c.Apply(t.Context(), &reconcilepb.ApplyRequest{Project: "demo", Model: []byte(okModel)})
	assert.NilError(t, err)
}

func (e *Extension) setActive(eng engine) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.active = eng
}
