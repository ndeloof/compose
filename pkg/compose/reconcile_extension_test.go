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
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	extensionclient "github.com/moby/extensions/client"
	"github.com/moby/moby/client"
	"go.uber.org/mock/gomock"
	"go.yaml.in/yaml/v4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"

	reconcilev0 "github.com/docker/compose/v5/internal/reconcileapi"
	reconcilepb "github.com/docker/compose/v5/internal/reconcileapi/protogen"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/mocks"
)

// fakeReconciler is the daemon's extension: it records what it receives and
// answers what the test scripted.
type fakeReconciler struct {
	mu sync.Mutex

	admit      []*reconcilev0.AdmitRequest
	apply      []*reconcilev0.ApplyRequest
	remove     []*reconcilev0.RemoveRequest
	refusals   []reconcilev0.Refusal
	admitErr   error
	applyErr   error
	removeErr  error
	applySteps []reconcilev0.Step
}

func (f *fakeReconciler) Admit(_ context.Context, req *reconcilev0.AdmitRequest) (*reconcilev0.AdmitReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admit = append(f.admit, req)
	return &reconcilev0.AdmitReply{Refusals: f.refusals}, f.admitErr
}

func (f *fakeReconciler) Apply(_ context.Context, req *reconcilev0.ApplyRequest) (*reconcilev0.ApplyReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apply = append(f.apply, req)
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return &reconcilev0.ApplyReply{Steps: f.applySteps}, nil
}

func (f *fakeReconciler) Remove(_ context.Context, req *reconcilev0.RemoveRequest) (*reconcilev0.RemoveReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remove = append(f.remove, req)
	if f.removeErr != nil {
		return nil, f.removeErr
	}
	return &reconcilev0.RemoveReply{Steps: f.applySteps}, nil
}

type bufEngine struct{ lis *bufconn.Listener }

func (e bufEngine) Dialer() func(context.Context) (net.Conn, error) { return e.lis.DialContext }

// serveReconciler serves fake over gRPC, with the generated wire, on an
// in-memory listener, and returns the client the composeService resolves.
func serveReconciler(t *testing.T, fake *fakeReconciler) daemonReconciler {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	reconcilepb.ServerPoint.Register(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	exts, err := extensionclient.New(bufEngine{lis}, extensionclient.WithGRPCPoint(reconcilepb.ClientPoint))
	assert.NilError(t, err)
	t.Cleanup(func() { _ = exts.Close() })
	rec, err := extensionclient.Resolve(exts, reconcilev0.Point)
	assert.NilError(t, err)
	return rec
}

// newExtensionService is a composeService that delegates to fake. enabled
// tells whether WithReconcileExtension was applied.
func newExtensionService(t *testing.T, fake *fakeReconciler, enabled bool, events api.EventProcessor) (*composeService, *mocks.MockAPIClient) {
	t.Helper()
	mockCtrl := gomock.NewController(t)
	apiClient, tested := newTestComposeService(t, mockCtrl, "1.48")
	if events != nil {
		tested.events = events
	}
	if enabled {
		assert.NilError(t, WithReconcileExtension(tested))
	}
	tested.reconcileExt.once.Do(func() {})
	tested.reconcileExt.api = serveReconciler(t, fake)
	return tested, apiClient
}

func detachedUp() api.UpOptions {
	return api.UpOptions{Create: api.CreateOptions{Inherit: true}}
}

func TestUpViaExtensionSendsCanonicalModel(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret.txt")
	assert.NilError(t, os.WriteFile(secretFile, []byte("s3cr$t"), 0o600))
	configFile := filepath.Join(dir, "app.conf")
	assert.NilError(t, os.WriteFile(configFile, []byte("key=value\n"), 0o600))

	fake := &fakeReconciler{}
	events := &capturingEvents{}
	tested, apiClient := newExtensionService(t, fake, true, events)
	// no pull: the image is local and its policy is the default
	apiClient.EXPECT().ImageInspect(anyCancellableContext(), "nginx:1", gomock.Any()).
		Return(client.ImageInspectResult{}, nil).AnyTimes()

	project := &types.Project{
		Name: "demo",
		Services: types.Services{
			"web": {
				Name: "web",
				ContainerSpec: types.ContainerSpec{
					Image:   "nginx:1",
					Secrets: []types.ServiceSecretConfig{{Source: "token"}},
					Configs: []types.ServiceConfigObjConfig{{Source: "conf"}},
				},
			},
		},
		Secrets: types.Secrets{"token": {File: secretFile}},
		Configs: types.Configs{"conf": {File: configFile}},
	}
	// what `docker compose config` would still carry: a leftover build section
	web := project.Services["web"]
	web.Build = &types.BuildConfig{Context: dir}
	project.Services["web"] = web

	options := detachedUp()
	options.Create.Services = []string{"web"}
	options.Create.Recreate = api.RecreateForce
	options.Create.RemoveOrphans = true
	options.Start.Wait = true
	options.Start.WaitTimeout = 90 * time.Second
	fake.applySteps = []reconcilev0.Step{
		{Operation: "create_network", Resource: "demo_default"},
		{Operation: "create_container", Resource: "demo-web-1", Service: "web"},
		{Operation: "start_container", Resource: "demo-web-1", Service: "web"},
	}

	assert.NilError(t, tested.Up(t.Context(), project, options))

	// admitted before applied, on the same model
	assert.Equal(t, len(fake.admit), 1)
	assert.Equal(t, len(fake.apply), 1)
	req := fake.apply[0]
	assert.DeepEqual(t, fake.admit[0].Model, req.Model)
	assert.Equal(t, fake.admit[0].Project, "demo")

	// option mapping
	assert.Equal(t, req.Project, "demo")
	assert.DeepEqual(t, req.Services, []string{"web"})
	assert.Equal(t, req.Recreate, reconcilev0.RecreateForce)
	assert.Equal(t, req.RemoveOrphans, true)
	assert.Equal(t, req.Wait, true)
	assert.Equal(t, req.WaitTimeoutSeconds, int64(90))
	assert.Equal(t, req.DryRun, false)

	// canonical model: no build, files inlined, pull left to the client
	var model map[string]any
	assert.NilError(t, yaml.Unmarshal(req.Model, &model))
	web2 := model["services"].(map[string]any)["web"].(map[string]any)
	assert.Assert(t, is.Nil(web2["build"]))
	assert.Equal(t, web2["image"], "nginx:1")
	assert.Equal(t, web2["pull_policy"], "never")
	token := model["secrets"].(map[string]any)["token"].(map[string]any)
	assert.Assert(t, is.Nil(token["file"]))
	// `$` is escaped like `docker compose config` does
	assert.Equal(t, token["content"], "s3cr$$t")
	conf := model["configs"].(map[string]any)["conf"].(map[string]any)
	assert.Assert(t, is.Nil(conf["file"]))
	assert.Equal(t, conf["content"], "key=value\n")

	// the project given by the caller is left as it was
	assert.Assert(t, project.Services["web"].Build != nil)
	assert.Equal(t, project.Secrets["token"].File, secretFile)

	// the plan is replayed as the events of a local reconciliation
	var texts []string
	for _, r := range events.resources {
		texts = append(texts, r.ID+" "+r.Text)
	}
	assert.DeepEqual(t, texts, []string{
		"Network demo_default Creating", "Network demo_default Created",
		"Container demo-web-1 Creating", "Container demo-web-1 Created",
		"Container demo-web-1 Starting", "Container demo-web-1 Started",
	})
}

func TestUpViaExtensionMakesBuiltImageNameExplicit(t *testing.T) {
	project := &types.Project{
		Name: "demo",
		Services: types.Services{
			"app": {Name: "app", WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: "."}}},
		},
	}
	content, err := canonicalReconcileModel(project)
	assert.NilError(t, err)
	var model map[string]any
	assert.NilError(t, yaml.Unmarshal(content, &model))
	app := model["services"].(map[string]any)["app"].(map[string]any)
	assert.Equal(t, app["image"], "demo-app")
	assert.Assert(t, is.Nil(app["build"]))
}

func TestInlineFileObjects(t *testing.T) {
	project := &types.Project{
		Environment: types.Mapping{"TOKEN": "from-env"},
		Secrets: types.Secrets{
			"missing":  {File: filepath.Join(t.TempDir(), "nope")},
			"external": {External: true},
		},
	}
	err := inlineFileObjects(project)
	assert.ErrorContains(t, err, `secret "missing"`)

	delete(project.Secrets, "missing")
	project.Secrets["env"] = types.SecretConfig{Environment: "TOKEN"}
	project.Configs = types.Configs{"unset": {Environment: "NOPE"}}
	assert.ErrorContains(t, inlineFileObjects(project), `config "unset": environment variable "NOPE" is not set`)

	delete(project.Configs, "unset")
	assert.NilError(t, inlineFileObjects(project))
	assert.Equal(t, project.Secrets["env"].Content, "from-env")
	assert.Equal(t, project.Secrets["env"].Environment, "")
	assert.Equal(t, bool(project.Secrets["external"].External), true)
}

func TestApplyRequestOptions(t *testing.T) {
	tests := []struct {
		name    string
		options api.UpOptions
		dryRun  bool
		want    reconcilev0.ApplyRequest
	}{
		{
			name: "defaults",
			want: reconcilev0.ApplyRequest{Project: "p", Model: []byte("m"), Recreate: reconcilev0.RecreateDiverged},
		},
		{
			name:    "no-recreate",
			options: api.UpOptions{Create: api.CreateOptions{Recreate: api.RecreateNever}},
			want:    reconcilev0.ApplyRequest{Project: "p", Model: []byte("m"), Recreate: reconcilev0.RecreateNever},
		},
		{
			name: "wait with a sub-second timeout rounds up, never to unbounded",
			options: api.UpOptions{Start: api.StartOptions{
				Wait: true, WaitTimeout: 1500 * time.Millisecond,
			}},
			want: reconcilev0.ApplyRequest{Project: "p", Model: []byte("m"), Recreate: reconcilev0.RecreateDiverged, Wait: true, WaitTimeoutSeconds: 2},
		},
		{
			name:    "wait timeout without wait is not sent",
			options: api.UpOptions{Start: api.StartOptions{WaitTimeout: time.Minute}},
			want:    reconcilev0.ApplyRequest{Project: "p", Model: []byte("m"), Recreate: reconcilev0.RecreateDiverged},
		},
		{
			name:   "dry run",
			dryRun: true,
			want:   reconcilev0.ApplyRequest{Project: "p", Model: []byte("m"), Recreate: reconcilev0.RecreateDiverged, DryRun: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.DeepEqual(t, *applyRequest("p", []byte("m"), tt.options, tt.dryRun), tt.want)
		})
	}
}

func TestUpViaExtensionRefusesWhatNeedsAClientSession(t *testing.T) {
	project := &types.Project{Name: "demo", Services: types.Services{"web": {Name: "web", ContainerSpec: types.ContainerSpec{Image: "nginx"}}}}
	tests := []struct {
		name    string
		mutate  func(*types.Project, *api.UpOptions)
		wantErr string
	}{
		{"attach", func(_ *types.Project, o *api.UpOptions) { o.Start.Attach = nopConsumer{} }, "up without --detach"},
		{"watch", func(_ *types.Project, o *api.UpOptions) { o.Start.Attach, o.Start.Watch = nopConsumer{}, true }, "--watch"},
		{"models", func(p *types.Project, _ *api.UpOptions) { p.Models = types.Models{"m": {}} }, "models"},
		{"provider", func(p *types.Project, _ *api.UpOptions) {
			s := p.Services["web"]
			s.Provider = &types.ServiceProviderConfig{Type: "x"}
			p.Services["web"] = s
		}, "provider services (web)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeReconciler{}
			tested, _ := newExtensionService(t, fake, true, nil)
			p, err := project.WithServicesTransform(func(_ string, s types.ServiceConfig) (types.ServiceConfig, error) { return s, nil })
			assert.NilError(t, err)
			options := detachedUp()
			tt.mutate(p, &options)
			err = tested.Up(t.Context(), p, options)
			assert.ErrorContains(t, err, tt.wantErr)
			assert.ErrorContains(t, err, unsupportedWithReconcileExtension)
			assert.Equal(t, len(fake.admit)+len(fake.apply), 0, "nothing reaches the daemon")
		})
	}
}

type nopConsumer struct{}

func (nopConsumer) Log(string, string)    {}
func (nopConsumer) Err(string, string)    {}
func (nopConsumer) Status(string, string) {}

func TestReconcileErrorsAreTranslated(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr string
	}{
		{"unimplemented", status.Error(codes.Unimplemented, "unknown service"), "the daemon does not provide the Compose reconcile extension"},
		{"unavailable", status.Error(codes.Unavailable, "starting"), "not ready: starting"},
		{"invalid", status.Error(codes.InvalidArgument, "services.web.image: required"), "services.web.image: required"},
		{"halfway", status.Error(codes.FailedPrecondition, "network gone"), "failed halfway, the daemon is left as far as it went: network gone"},
		{"timeout", status.Error(codes.DeadlineExceeded, "web not healthy"), "timeout waiting for the services: web not healthy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeReconciler{applyErr: tt.err}
			tested, _ := newExtensionService(t, fake, true, nil)
			project := &types.Project{Name: "demo", Services: types.Services{}}
			err := tested.Up(t.Context(), project, detachedUp())
			assert.ErrorContains(t, err, tt.wantErr)
			assert.Assert(t, !errors.Is(err, context.Canceled))
		})
	}

	t.Run("canceled", func(t *testing.T) {
		assert.Assert(t, errors.Is(translateReconcileError(status.Error(codes.Canceled, "gone")), context.Canceled))
	})
	t.Run("not a status", func(t *testing.T) {
		boom := errors.New("boom")
		assert.Equal(t, translateReconcileError(boom), boom)
	})
}

func TestUpViaExtensionReportsRefusals(t *testing.T) {
	fake := &fakeReconciler{refusals: []reconcilev0.Refusal{
		{Path: "services.web.volumes[0]", Reason: "bind mount of a client path"},
		{Path: "services.db.build", Reason: "build is the client's business"},
	}}
	tested, _ := newExtensionService(t, fake, true, nil)
	project := &types.Project{Name: "demo", Services: types.Services{}}
	err := tested.Up(t.Context(), project, detachedUp())
	assert.Error(t, err, "the daemon's reconcile extension cannot apply this model:\n"+
		" - services.web.volumes[0]: bind mount of a client path\n"+
		" - services.db.build: build is the client's business")
	assert.Equal(t, len(fake.apply), 0, "a refused model is not applied")
}

func TestDownViaExtension(t *testing.T) {
	fake := &fakeReconciler{applySteps: []reconcilev0.Step{
		{Operation: "stop_container", Resource: "demo-web-1"},
		{Operation: "remove_container", Resource: "demo-web-1"},
		{Operation: "remove_network", Resource: "demo_default"},
		{Operation: "remove_volume", Resource: "demo_data"},
		{Operation: "rename_container", Resource: "ignored"},
	}}
	events := &capturingEvents{}
	tested, _ := newExtensionService(t, fake, true, events)
	timeout := 2500 * time.Millisecond

	err := tested.Down(t.Context(), "Demo", api.DownOptions{Volumes: true, RemoveOrphans: true, Timeout: &timeout})
	assert.NilError(t, err)
	assert.DeepEqual(t, fake.remove, []*reconcilev0.RemoveRequest{{
		Project: "demo", Volumes: true, RemoveOrphans: true, TimeoutSeconds: 3,
	}})
	var texts []string
	for _, r := range events.resources {
		texts = append(texts, r.ID+" "+r.Text)
	}
	assert.DeepEqual(t, texts, []string{
		"Container demo-web-1 Stopping", "Container demo-web-1 Stopped",
		"Container demo-web-1 Removing", "Container demo-web-1 Removed",
		"Network demo_default Removing", "Network demo_default Removed",
		"Volume demo_data Removing", "Volume demo_data Removed",
	})

	t.Run("error", func(t *testing.T) {
		fake := &fakeReconciler{removeErr: status.Error(codes.Unimplemented, "nope")}
		tested, _ := newExtensionService(t, fake, true, nil)
		err := tested.Down(t.Context(), "demo", api.DownOptions{})
		assert.ErrorContains(t, err, "the daemon does not provide the Compose reconcile extension")
	})

	t.Run("unsupported options", func(t *testing.T) {
		fake := &fakeReconciler{}
		tested, _ := newExtensionService(t, fake, true, nil)
		err := tested.Down(t.Context(), "demo", api.DownOptions{Images: api.ImagePruneLocal, Services: []string{"web"}})
		assert.ErrorContains(t, err, "--rmi, down of selected services: "+unsupportedWithReconcileExtension)
		assert.Equal(t, len(fake.remove), 0)
	})
}

// With the extension off, Down is the engine-side one: it reads the daemon's
// state, and the extension is never asked.
func TestDownWithoutExtensionIsUnchanged(t *testing.T) {
	fake := &fakeReconciler{}
	tested, apiClient := newExtensionService(t, fake, false, nil)
	listed := errors.New("container list reached")
	apiClient.EXPECT().ContainerList(gomock.Any(), gomock.Any()).Return(client.ContainerListResult{}, listed)

	err := tested.Down(t.Context(), "demo", api.DownOptions{})
	assert.Assert(t, errors.Is(err, listed), "got %v", err)
	assert.Equal(t, len(fake.remove), 0)
}

// The extension is off unless asked for: no option, no resolution.
func TestReconcileExtensionIsOffByDefault(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	_, tested := newTestComposeService(t, mockCtrl, "1.48")
	assert.Assert(t, !tested.reconcileExt.enabled)
	assert.Assert(t, tested.reconcileExt.client == nil)
}
