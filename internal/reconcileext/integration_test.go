//go:build integration

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

// End-to-end test of the extension inside a real daemon.
//
// It needs a daemon built from moby master with the extension mechanism and the
// Compose binary installed in its extension directory: see
// Dockerfile.reconcile-dind at the root of the repository.
//
//	docker buildx build -f Dockerfile.reconcile-dind -t compose-reconcile-dind:dev .
//	docker run -d --privileged --name reconcile-ext-e2e -e DOCKER_TLS_CERTDIR= -p 23751:2375 compose-reconcile-dind:dev
//	DOCKER_HOST=tcp://127.0.0.1:23751 go test -tags integration ./internal/reconcileext/ -run TestIntegration -v
//
// The test plays the client: it only talks to the daemon's socket, through the
// generated client of the point (what the Compose CLI does). The images are
// pulled by the daemon, without credentials.

import (
	"strings"
	"testing"
	"time"

	extensionclient "github.com/moby/extensions/client"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"gotest.tools/v3/assert"

	"github.com/docker/compose/v5/internal/naming"
	reconcilev0 "github.com/docker/compose/v5/internal/reconcileapi"
	reconcilepb "github.com/docker/compose/v5/internal/reconcileapi/protogen"
)

const integrationProject = "reconcile-it"

// integrationModel is a model as the client sends it: what `docker compose
// config` prints (with `$` escaped as `$$`), no build, explicit images.
func integrationModel(webGreeting string) []byte {
	return []byte(`
name: reconcile-it
services:
  db:
    image: alpine:3.22
    pull_policy: missing
    command: ["sleep", "3600"]
    environment:
      PRICE: 5$$
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      retries: 3
    networks:
      back: {}
    volumes:
      - type: volume
        source: data
        target: /data
  web:
    image: nginx:1.29-alpine
    pull_policy: missing
    environment:
      GREETING: ` + webGreeting + `
    depends_on:
      db:
        condition: service_healthy
        required: true
    networks:
      back: {}
networks:
  back:
    name: reconcile-it_back
volumes:
  data:
    name: reconcile-it_data
`)
}

func TestIntegration(t *testing.T) {
	engine, err := client.New(client.FromEnv)
	assert.NilError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	exts, err := extensionclient.New(engine, extensionclient.WithGRPCPoint(reconcilepb.ClientPoint))
	assert.NilError(t, err)
	t.Cleanup(func() { _ = exts.Close() })
	reconciler, err := extensionclient.Resolve(exts, reconcilev0.Point)
	assert.NilError(t, err)

	ctx := t.Context()
	// the extension answers Unavailable until the daemon serves its API
	waitActive(t, reconciler)
	t.Cleanup(func() {
		_, _ = reconciler.Remove(ctx, &reconcilev0.RemoveRequest{Project: integrationProject, Volumes: true})
	})

	containers := func() map[string]string { // service -> ID
		list, err := engine.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: naming.ProjectFilter(integrationProject)})
		assert.NilError(t, err)
		out := map[string]string{}
		for _, c := range list.Items {
			out[c.Labels["com.docker.compose.service"]] = c.ID
		}
		return out
	}

	t.Run("admit", func(t *testing.T) {
		ok, err := reconciler.Admit(ctx, &reconcilev0.AdmitRequest{Project: integrationProject, Model: integrationModel("hi")})
		assert.NilError(t, err)
		assert.Equal(t, len(ok.Refusals), 0)

		refused, err := reconciler.Admit(ctx, &reconcilev0.AdmitRequest{Project: integrationProject, Model: []byte("services:\n  a:\n    image: x\n    build: .\n")})
		assert.NilError(t, err)
		assert.Equal(t, len(refused.Refusals), 1)
		assert.Equal(t, refused.Refusals[0].Path, "services.a.build")
	})

	t.Run("dry run changes nothing", func(t *testing.T) {
		reply, err := reconciler.Apply(ctx, &reconcilev0.ApplyRequest{Project: integrationProject, Model: integrationModel("hi"), DryRun: true})
		assert.NilError(t, err)
		assert.Assert(t, hasStep(reply.Steps, "create_container", "reconcile-it-web-1"), "%v", reply.Steps)
		assert.Equal(t, len(containers()), 0)
	})

	var dbID, webID string
	t.Run("apply creates", func(t *testing.T) {
		reply, err := reconciler.Apply(ctx, &reconcilev0.ApplyRequest{
			Project:            integrationProject,
			Model:              integrationModel("hi"),
			WorkingDir:         "/home/me/app",
			ConfigFiles:        []string{"/home/me/app/compose.yaml"},
			Wait:               true,
			WaitTimeoutSeconds: 180,
		})
		assert.NilError(t, err)
		assert.Assert(t, hasStep(reply.Steps, "create_network", "reconcile-it_back"), "%v", reply.Steps)
		assert.Assert(t, hasStep(reply.Steps, "create_volume", "reconcile-it_data"), "%v", reply.Steps)
		assert.Assert(t, hasStep(reply.Steps, "create_container", "reconcile-it-db-1"), "%v", reply.Steps)
		assert.Assert(t, hasStep(reply.Steps, "create_container", "reconcile-it-web-1"), "%v", reply.Steps)
		assert.Equal(t, len(reply.Services), 2)
		for _, s := range reply.Services {
			assert.Equal(t, s.Running, uint32(1), s.Name)
		}

		ids := containers()
		assert.Equal(t, len(ids), 2)
		dbID, webID = ids["db"], ids["web"]
		_, err = engine.NetworkInspect(ctx, "reconcile-it_back", client.NetworkInspectOptions{})
		assert.NilError(t, err)
		_, err = engine.VolumeInspect(ctx, "reconcile-it_data", client.VolumeInspectOptions{})
		assert.NilError(t, err)

		// the labels of a regular project: a filter on them finds the containers
		labels := inspectContainer(t, engine, dbID).Config.Labels
		for _, l := range []string{"project", "service", "version", "oneoff", "project.working_dir", "project.config_files"} {
			assert.Assert(t, labels["com.docker.compose."+l] != "", "label %s: %v", l, labels)
		}
		netInspect, err := engine.NetworkInspect(ctx, "reconcile-it_back", client.NetworkInspectOptions{})
		assert.NilError(t, err)
		assert.Assert(t, netInspect.Network.Labels["com.docker.compose.version"] != "", "the version label of the network")

		// `$$` of the canonical document reached the container as `$`
		inspect, err := engine.ContainerInspect(ctx, dbID, client.ContainerInspectOptions{})
		assert.NilError(t, err)
		assert.Assert(t, contains(inspect.Container.Config.Env, "PRICE=5$"), "%v", inspect.Container.Config.Env)
		assert.Assert(t, inspect.Container.State.Running)
	})

	t.Run("apply again is idempotent", func(t *testing.T) {
		reply, err := reconciler.Apply(ctx, &reconcilev0.ApplyRequest{Project: integrationProject, Model: integrationModel("hi"), Wait: true, WaitTimeoutSeconds: 60})
		assert.NilError(t, err)
		for _, s := range reply.Steps {
			assert.Assert(t, !strings.HasPrefix(s.Operation, "create_") && !strings.HasPrefix(s.Operation, "remove_") && s.Operation != "stop_container",
				"unexpected step %v", s)
		}
		ids := containers()
		assert.Equal(t, ids["db"], dbID)
		assert.Equal(t, ids["web"], webID)
	})

	t.Run("a modified service is recreated alone", func(t *testing.T) {
		reply, err := reconciler.Apply(ctx, &reconcilev0.ApplyRequest{Project: integrationProject, Model: integrationModel("changed"), Wait: true, WaitTimeoutSeconds: 120})
		assert.NilError(t, err)
		assert.Assert(t, hasStep(reply.Steps, "create_container", "reconcile-it-web-1"), "%v", reply.Steps)
		ids := containers()
		assert.Equal(t, ids["db"], dbID, "db must be left alone")
		assert.Assert(t, ids["web"] != webID, "web must be recreated")
		inspect, err := engine.ContainerInspect(ctx, ids["web"], client.ContainerInspectOptions{})
		assert.NilError(t, err)
		assert.Assert(t, contains(inspect.Container.Config.Env, "GREETING=changed"))
	})

	t.Run("remove", func(t *testing.T) {
		reply, err := reconciler.Remove(ctx, &reconcilev0.RemoveRequest{Project: integrationProject, Volumes: true})
		assert.NilError(t, err)
		assert.Assert(t, hasStep(reply.Steps, "remove_container", "reconcile-it-web-1"), "%v", reply.Steps)
		assert.Assert(t, hasStep(reply.Steps, "remove_network", "reconcile-it_back"), "%v", reply.Steps)
		assert.Assert(t, hasStep(reply.Steps, "remove_volume", "reconcile-it_data"), "%v", reply.Steps)
		assert.Equal(t, len(containers()), 0)
		_, err = engine.NetworkInspect(ctx, "reconcile-it_back", client.NetworkInspectOptions{})
		assert.Assert(t, err != nil, "network must be gone")
		_, err = engine.VolumeInspect(ctx, "reconcile-it_data", client.VolumeInspectOptions{})
		assert.Assert(t, err != nil, "volume must be gone")
	})
}

func waitActive(t *testing.T, r reconcilev0.Reconciler) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err := r.Remove(t.Context(), &reconcilev0.RemoveRequest{Project: integrationProject})
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reconciler never became available: %v", err)
		}
		time.Sleep(time.Second)
	}
}

func hasStep(steps []reconcilev0.Step, operation, resource string) bool {
	for _, s := range steps {
		if s.Operation == operation && s.Resource == resource {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func inspectContainer(t *testing.T, engine *client.Client, id string) container.InspectResponse {
	t.Helper()
	res, err := engine.ContainerInspect(t.Context(), id, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	return res.Container
}
