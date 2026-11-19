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
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	reconcilev0 "github.com/docker/compose/v5/internal/reconcileapi"
	"github.com/docker/compose/v5/pkg/api"
)

// errModel marks the errors that come from the model the caller sent, as
// opposed to those of the convergence: they map to InvalidArgument.
var errModel = errors.New("invalid model")

// loadModel reads the canonical Compose document the client sent.
//
// The document is what `docker compose config` prints, so everything the
// client could resolve is: this is not a regular project load. Nothing is read
// from disk or from the environment: no include, no extends, no env_file, an
// empty environment, no relative path resolution, every profile enabled (the
// client already selected the services it wants).
//
// The client's document carries secrets with their `content` next to the
// `file` they come from, which the compose schema does not allow for secrets:
// the schema validation is skipped (the client validated the document when it
// loaded it; the consistency checks of the loader still run).
//
// Interpolation stays on because the client escapes `$` as `$$`, exactly as
// `docker compose config` does: reading the document back unescapes it.
func loadModel(ctx context.Context, name string, model []byte) (*types.Project, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: project name is required", errModel)
	}
	if len(model) == 0 {
		return nil, fmt.Errorf("%w: model is required", errModel)
	}
	details := types.ConfigDetails{
		WorkingDir:  "/",
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yaml", Content: model}},
		Environment: types.Mapping{},
	}
	project, err := loader.LoadWithContext(ctx, details, func(o *loader.Options) {
		o.SetProjectName(name, true)
		o.SkipInclude = true
		o.SkipExtends = true
		o.SkipResolveEnvironment = true
		o.ResolvePaths = false
		// the schema has no `content` for a secret, which is how the client
		// hands one over (`config` prints it next to `file`)
		o.SkipValidation = true
		o.Profiles = []string{"*"}
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", errModel, err)
	}
	return project, nil
}

// labelServices gives every service the labels that tie its containers to the
// project, which the regular loader adds (see projectloader) and the canonical
// document does not carry. workingDir and configFiles only exist on the client:
// the labels they feed are left unset when it did not send them.
func labelServices(project *types.Project, workingDir string, configFiles []string) {
	for name, svc := range project.Services {
		svc.CustomLabels = types.Labels{
			api.ProjectLabel: project.Name,
			api.ServiceLabel: name,
			api.VersionLabel: api.ComposeVersion,
			api.OneoffLabel:  "False",
		}
		if workingDir != "" {
			svc.CustomLabels[api.WorkingDirLabel] = workingDir
		}
		if len(configFiles) > 0 {
			svc.CustomLabels[api.ConfigFilesLabel] = strings.Join(configFiles, ",")
		}
		project.Services[name] = svc
	}
}

// admit lists what the project holds that cannot run in the daemon, each with
// its path in the model. The list is deliberately short: it is what only the
// client can do, and that the client has to do before delegating.
//
//   - services.X.build: the build stays on the client, which hands the built
//     image over by name (`image`, with a `pull_policy` that does not pull);
//   - services.X.provider: provider plugins are binaries of the client;
//   - models, services.X.models: models are run by the `docker model` CLI;
//   - secrets.X.file / secrets.X.environment, same for configs, when there is
//     no `content` beside them: they read the client's files and environment,
//     the client must inline them as `content`;
//   - services.X.use_api_socket: relies on the client's credentials;
//   - services.X.env_file, services.X.label_file: files of the client, to inline
//     as `environment` and `labels`;
//   - bind mounts with a relative source: relative to the client's directory.
//
// `develop` is ignored (watch is a client feature), not refused.
func admit(project *types.Project) []reconcilev0.Refusal {
	var refusals []reconcilev0.Refusal
	refuse := func(path, reason string) {
		refusals = append(refusals, reconcilev0.Refusal{Path: path, Reason: reason})
	}

	for _, name := range slices.Sorted(maps.Keys(project.Models)) {
		refuse("models."+name, "models are run by the client: they cannot be applied by the daemon")
	}
	for _, name := range slices.Sorted(maps.Keys(project.Secrets)) {
		refuseFileObject("secrets."+name, types.FileObjectConfig(project.Secrets[name]), refuse)
	}
	for _, name := range slices.Sorted(maps.Keys(project.Configs)) {
		refuseFileObject("configs."+name, types.FileObjectConfig(project.Configs[name]), refuse)
	}

	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		svc := project.Services[name]
		prefix := "services." + name
		if svc.Build != nil {
			refuse(prefix+".build", "the build stays on the client: build the image there and hand the service over with `image` only")
		}
		if svc.Provider != nil {
			refuse(prefix+".provider", "provider services run plugins of the client")
		}
		if len(svc.Models) > 0 {
			refuse(prefix+".models", "models are run by the client: they cannot be applied by the daemon")
		}
		if svc.UseAPISocket {
			refuse(prefix+".use_api_socket", "relies on the credentials of the client")
		}
		if len(svc.EnvFiles) > 0 {
			refuse(prefix+".env_file", "files of the client are not readable by the daemon: inline them in `environment`")
		}
		if len(svc.LabelFiles) > 0 {
			refuse(prefix+".label_file", "files of the client are not readable by the daemon: inline them in `labels`")
		}
		for i, v := range svc.Volumes {
			if v.Type == types.VolumeTypeBind && v.Source != "" && !filepath.IsAbs(v.Source) && !strings.HasPrefix(v.Source, "/") {
				refuse(fmt.Sprintf("%s.volumes[%d]", prefix, i), "a relative bind mount is relative to the client: resolve it to an absolute path of the daemon's host")
			}
		}
	}
	return refusals
}

// refuseFileObject refuses a secret or config that takes its content from the
// client.
func refuseFileObject(path string, obj types.FileObjectConfig, refuse func(path, reason string)) {
	// the content wins over the file it comes from: it is inlined
	if obj.Content != "" {
		return
	}
	if obj.File != "" {
		refuse(path+".file", "files of the client are not readable by the daemon: inline the content with `content`")
	}
	if obj.Environment != "" {
		refuse(path+".environment", "the environment of the client is not available to the daemon: inline the content with `content`")
	}
}

// refusalsError is the error of a model that cannot be applied: it lists every
// refusal with its path.
func refusalsError(refusals []reconcilev0.Refusal) error {
	parts := make([]string, len(refusals))
	for i, r := range refusals {
		parts[i] = r.Path + ": " + r.Reason
	}
	return fmt.Errorf("%w: %s", errModel, strings.Join(parts, "; "))
}
