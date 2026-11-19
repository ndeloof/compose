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
	"errors"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"gotest.tools/v3/assert"

	reconcilev0 "github.com/docker/compose/v5/internal/reconcileapi"
	"github.com/docker/compose/v5/pkg/api"
)

const okModel = `
name: demo
services:
  db:
    image: postgres:17
    pull_policy: never
    environment:
      PASSWORD: pa$$word
  web:
    image: nginx
    pull_policy: never
    depends_on:
      db:
        condition: service_started
    volumes:
      - type: volume
        source: data
        target: /data
      - type: bind
        source: /var/run/docker.sock
        target: /sock
    develop:
      watch:
        - action: sync
          path: ./src
          target: /app
volumes:
  data: {}
secrets:
  token:
    file: /home/client/token
    content: s3cret
`

func TestAdmitAcceptsAClientResolvedModel(t *testing.T) {
	project, err := loadModel(t.Context(), "demo", []byte(okModel))
	assert.NilError(t, err)
	assert.Equal(t, len(admit(project)), 0)

	// `$$` is the escaping of the canonical document: read back as a `$`
	assert.Equal(t, *project.Services["db"].Environment["PASSWORD"], "pa$word")
	assert.Equal(t, project.Name, "demo")
}

func TestAdmitRefusals(t *testing.T) {
	model := `
services:
  api:
    image: busybox
    use_api_socket: true
    env_file: [/tmp/app.env]
    models:
      llm: {}
    volumes:
      - type: bind
        source: ./rel
        target: /rel
  built:
    build: /src
  ext:
    provider:
      type: awesome
models:
  llm:
    model: ai/smollm2
secrets:
  from_file:
    file: /etc/secret
  from_env:
    environment: TOKEN
  inline:
    file: /home/client/inline
    content: x
configs:
  conf:
    file: /etc/conf
`
	// the loader refuses what is not a valid model by itself (a relative bind
	// source stays relative: no path resolution)
	project, err := loadModel(t.Context(), "demo", []byte(model))
	assert.NilError(t, err)
	got := map[string]bool{}
	for _, r := range admit(project) {
		assert.Assert(t, r.Reason != "", r.Path)
		got[r.Path] = true
	}
	for _, path := range []string{
		"models.llm",
		"secrets.from_file.file",
		"secrets.from_env.environment",
		"configs.conf.file",
		"services.api.use_api_socket",
		"services.api.env_file",
		"services.api.models",
		"services.api.volumes[0]",
		"services.built.build",
		"services.ext.provider",
	} {
		assert.Assert(t, got[path], "missing refusal for %s, got %v", path, got)
	}
	assert.Assert(t, !got["secrets.inline.file"], "a secret with content is inlined")
	assert.Equal(t, len(got), 10)
}

func TestLoadModelErrorsAreModelErrors(t *testing.T) {
	for name, tt := range map[string]struct {
		project string
		model   string
	}{
		"no project":   {"", okModel},
		"no model":     {"demo", ""},
		"not yaml":     {"demo", "services: [unterminated"},
		"invalid type": {"demo", "services:\n  web:\n    image: nginx\n    ports: {not: a list}\n"},
		"unknown net":  {"demo", "services:\n  web:\n    image: nginx\n    networks: [nowhere]\n"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadModel(t.Context(), tt.project, []byte(tt.model))
			assert.Assert(t, errors.Is(err, errModel), "%v", err)
		})
	}
}

func TestRefusalsErrorListsEveryPath(t *testing.T) {
	err := refusalsError([]reconcilev0.Refusal{
		{Path: "services.a.build", Reason: "r1"},
		{Path: "models.m", Reason: "r2"},
	})
	assert.Error(t, err, "invalid model: services.a.build: r1; models.m: r2")
}

func TestLabelServices(t *testing.T) {
	project, err := loadModel(t.Context(), "demo", []byte(okModel))
	assert.NilError(t, err)

	// what the client did not send leaves its label unset
	labelServices(project, "", nil)
	assert.DeepEqual(t, project.Services["web"].CustomLabels, types.Labels{
		api.ProjectLabel: "demo", api.ServiceLabel: "web", api.VersionLabel: api.ComposeVersion, api.OneoffLabel: "False",
	})

	labelServices(project, "/home/me/app", []string{"/home/me/app/compose.yaml", "/home/me/app/override.yaml"})
	assert.DeepEqual(t, project.Services["db"].CustomLabels, types.Labels{
		api.ProjectLabel: "demo", api.ServiceLabel: "db", api.VersionLabel: api.ComposeVersion, api.OneoffLabel: "False",
		api.WorkingDirLabel:  "/home/me/app",
		api.ConfigFilesLabel: "/home/me/app/compose.yaml,/home/me/app/override.yaml",
	})
}
