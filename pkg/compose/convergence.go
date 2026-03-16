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
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	mmount "github.com/moby/moby/api/types/mount"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/docker/compose/v5/internal/tracing"
	"github.com/docker/compose/v5/pkg/api"
)

// convergence manages service's container lifecycle.
// Based on initially observed state, it reconciles the existing container with desired state, which might include
// re-creating container, adding or removing replicas, or starting stopped containers.
// Cross services dependencies are managed by creating services in expected order and updating `service:xx` reference
// when a service has converged, so dependent ones can be managed with resolved containers references.
type convergence struct {
	compose    *composeService
	services   map[string]Containers
	networks   map[string]string
	volumes    map[string]string
	stateMutex sync.Mutex
}

func (c *convergence) getObservedState(serviceName string) Containers {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	return c.services[serviceName]
}

func (c *convergence) setObservedState(serviceName string, containers Containers) {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	c.services[serviceName] = containers
}

func newConvergence(services []string, state Containers, networks map[string]string, volumes map[string]string, s *composeService) *convergence {
	observedState := map[string]Containers{}
	for _, s := range services {
		observedState[s] = Containers{}
	}
	for _, c := range state.filter(isNotOneOff) {
		service := c.Labels[api.ServiceLabel]
		observedState[service] = append(observedState[service], c)
	}
	return &convergence{
		compose:  s,
		services: observedState,
		networks: networks,
		volumes:  volumes,
	}
}

func (c *convergence) apply(ctx context.Context, project *types.Project, options api.CreateOptions) error {
	return InDependencyOrder(ctx, project, func(ctx context.Context, name string) error {
		service, err := project.GetService(name)
		if err != nil {
			return err
		}

		return tracing.SpanWrapFunc("service/apply", tracing.ServiceOptions(service), func(ctx context.Context) error {
			strategy := options.RecreateDependencies
			if slices.Contains(options.Services, name) {
				strategy = options.Recreate
			}
			return c.ensureService(ctx, project, service, strategy, options.Inherit, options.Timeout)
		})(ctx)
	})
}

func (c *convergence) ensureService(ctx context.Context, project *types.Project, service types.ServiceConfig, recreate string, inherit bool, timeout *time.Duration) error { //nolint:gocyclo
	if service.Provider != nil {
		return c.compose.runPlugin(ctx, project, service, "up")
	}
	expected, err := getScale(service)
	if err != nil {
		return err
	}
	containers := c.getObservedState(service.Name)
	actual := len(containers)
	updated := make(Containers, expected)

	eg, ctx := errgroup.WithContext(ctx)

	err = c.resolveServiceReferences(&service)
	if err != nil {
		return err
	}

	sort.Slice(containers, func(i, j int) bool {
		// select obsolete containers first, so they get removed as we scale down
		if obsolete, _ := c.mustRecreate(service, containers[i], recreate); obsolete {
			// i is obsolete, so must be first in the list
			return true
		}
		if obsolete, _ := c.mustRecreate(service, containers[j], recreate); obsolete {
			// j is obsolete, so must be first in the list
			return false
		}

		// For up-to-date containers, sort by container number to preserve low-values in container numbers
		ni, erri := strconv.Atoi(containers[i].Labels[api.ContainerNumberLabel])
		nj, errj := strconv.Atoi(containers[j].Labels[api.ContainerNumberLabel])
		if erri == nil && errj == nil {
			return ni > nj
		}

		// If we don't get a container number (?) just sort by creation date
		return containers[i].Created < containers[j].Created
	})

	slices.Reverse(containers)
	for i, ctr := range containers {
		if i >= expected {
			// Scale Down
			// As we sorted containers, obsolete ones and/or highest number will be removed
			ctr := ctr
			traceOpts := append(tracing.ServiceOptions(service), tracing.ContainerOptions(ctr)...)
			eg.Go(tracing.SpanWrapFuncForErrGroup(ctx, "service/scale/down", traceOpts, func(ctx context.Context) error {
				return c.compose.stopAndRemoveContainer(ctx, ctr, &service, timeout, false)
			}))
			continue
		}

		mustRecreate, err := c.mustRecreate(service, ctr, recreate)
		if err != nil {
			return err
		}
		if mustRecreate {
			err := c.stopDependentContainers(ctx, project, service)
			if err != nil {
				return err
			}

			i, ctr := i, ctr
			eg.Go(tracing.SpanWrapFuncForErrGroup(ctx, "container/recreate", tracing.ContainerOptions(ctr), func(ctx context.Context) error {
				recreated, err := c.compose.recreateContainer(ctx, project, service, ctr, inherit, timeout)
				updated[i] = recreated
				return err
			}))
			continue
		}

		// Enforce non-diverged containers are running
		name := getContainerProgressName(ctr)
		switch ctr.State {
		case container.StateRunning:
			c.compose.events.On(runningEvent(name))
		case container.StateCreated:
		case container.StateRestarting:
		case container.StateExited:
		default:
			ctr := ctr
			eg.Go(tracing.EventWrapFuncForErrGroup(ctx, "service/start", tracing.ContainerOptions(ctr), func(ctx context.Context) error {
				return c.compose.startContainer(ctx, ctr)
			}))
		}
		updated[i] = ctr
	}

	next := nextContainerNumber(containers)
	for i := 0; i < expected-actual; i++ {
		// Scale UP
		number := next + i
		name := getContainerName(project.Name, service, number)
		eventOpts := tracing.SpanOptions{trace.WithAttributes(attribute.String("container.name", name))}
		eg.Go(tracing.EventWrapFuncForErrGroup(ctx, "service/scale/up", eventOpts, func(ctx context.Context) error {
			opts := createOptions{
				AutoRemove:        false,
				AttachStdin:       false,
				UseNetworkAliases: true,
				Labels:            mergeLabels(service.Labels, service.CustomLabels),
			}
			ctr, err := c.compose.createContainer(ctx, project, service, name, number, opts)
			updated[actual+i] = ctr
			return err
		}))
		continue
	}

	err = eg.Wait()
	c.setObservedState(service.Name, updated)
	return err
}

func (c *convergence) stopDependentContainers(ctx context.Context, project *types.Project, service types.ServiceConfig) error {
	// Stop dependent containers, so they will be restarted after service is re-created
	dependents := project.GetDependentsForService(service, func(dependency types.ServiceDependency) bool {
		return dependency.Restart
	})
	if len(dependents) == 0 {
		return nil
	}
	err := c.compose.stop(ctx, project.Name, api.StopOptions{
		Services: dependents,
		Project:  project,
	}, nil)
	if err != nil {
		return err
	}

	for _, name := range dependents {
		dependentStates := c.getObservedState(name)
		for i, dependent := range dependentStates {
			dependent.State = container.StateExited
			dependentStates[i] = dependent
		}
		c.setObservedState(name, dependentStates)
	}
	return nil
}

// resolveServiceReferences replaces reference to another service with reference to an actual container
func (c *convergence) resolveServiceReferences(service *types.ServiceConfig) error {
	err := c.resolveVolumeFrom(service)
	if err != nil {
		return err
	}

	err = c.resolveSharedNamespaces(service)
	if err != nil {
		return err
	}
	return nil
}

func (c *convergence) resolveVolumeFrom(service *types.ServiceConfig) error {
	for i, vol := range service.VolumesFrom {
		spec := strings.Split(vol, ":")
		if len(spec) == 0 {
			continue
		}
		if spec[0] == "container" {
			service.VolumesFrom[i] = spec[1]
			continue
		}
		name := spec[0]
		dependencies := c.getObservedState(name)
		if len(dependencies) == 0 {
			return fmt.Errorf("cannot share volume with service %s: container missing", name)
		}
		service.VolumesFrom[i] = dependencies.sorted()[0].ID
	}
	return nil
}

func (c *convergence) resolveSharedNamespaces(service *types.ServiceConfig) error {
	str := service.NetworkMode
	if name := getDependentServiceFromMode(str); name != "" {
		dependencies := c.getObservedState(name)
		if len(dependencies) == 0 {
			return fmt.Errorf("cannot share network namespace with service %s: container missing", name)
		}
		service.NetworkMode = types.ContainerPrefix + dependencies.sorted()[0].ID
	}

	str = service.Ipc
	if name := getDependentServiceFromMode(str); name != "" {
		dependencies := c.getObservedState(name)
		if len(dependencies) == 0 {
			return fmt.Errorf("cannot share IPC namespace with service %s: container missing", name)
		}
		service.Ipc = types.ContainerPrefix + dependencies.sorted()[0].ID
	}

	str = service.Pid
	if name := getDependentServiceFromMode(str); name != "" {
		dependencies := c.getObservedState(name)
		if len(dependencies) == 0 {
			return fmt.Errorf("cannot share PID namespace with service %s: container missing", name)
		}
		service.Pid = types.ContainerPrefix + dependencies.sorted()[0].ID
	}

	return nil
}

func (c *convergence) mustRecreate(expected types.ServiceConfig, actual container.Summary, policy string) (bool, error) {
	if policy == api.RecreateNever {
		return false, nil
	}
	if policy == api.RecreateForce {
		return true, nil
	}
	configHash, err := ServiceHash(expected)
	if err != nil {
		return false, err
	}
	configChanged := actual.Labels[api.ConfigHashLabel] != configHash
	imageUpdated := actual.Labels[api.ImageDigestLabel] != expected.CustomLabels[api.ImageDigestLabel]
	if configChanged || imageUpdated {
		return true, nil
	}

	if c.networks != nil && actual.State == "running" {
		if checkExpectedNetworks(expected, actual, c.networks) {
			return true, nil
		}
	}

	if c.volumes != nil {
		if checkExpectedVolumes(expected, actual, c.volumes) {
			return true, nil
		}
	}

	return false, nil
}

func checkExpectedNetworks(expected types.ServiceConfig, actual container.Summary, networks map[string]string) bool {
	// check the networks container is connected to are the expected ones
	for net := range expected.Networks {
		id := networks[net]
		if id == "swarm" {
			// corner-case : swarm overlay network isn't visible until a container is attached
			continue
		}
		found := false
		for _, settings := range actual.NetworkSettings.Networks {
			if settings.NetworkID == id {
				found = true
				break
			}
		}
		if !found {
			// config is up-to-date but container is not connected to network
			return true
		}
	}
	return false
}

func checkExpectedVolumes(expected types.ServiceConfig, actual container.Summary, volumes map[string]string) bool {
	// check container's volume mounts and search for the expected ones
	for _, vol := range expected.Volumes {
		if vol.Type != string(mmount.TypeVolume) {
			continue
		}
		if vol.Source == "" {
			continue
		}
		id := volumes[vol.Source]
		found := false
		for _, mount := range actual.Mounts {
			if mount.Type != mmount.TypeVolume {
				continue
			}
			if mount.Name == id {
				found = true
				break
			}
		}
		if !found {
			// config is up-to-date but container doesn't have volume mounted
			return true
		}
	}
	return false
}

func getContainerProgressName(ctr container.Summary) string {
	return "Container " + getCanonicalContainerName(ctr)
}

func containerEvents(containers Containers, eventFunc func(string) api.Resource) []api.Resource {
	events := []api.Resource{}
	for _, ctr := range containers {
		events = append(events, eventFunc(getContainerProgressName(ctr)))
	}
	return events
}

func containerReasonEvents(containers Containers, eventFunc func(string, string) api.Resource, reason string) []api.Resource {
	events := []api.Resource{}
	for _, ctr := range containers {
		events = append(events, eventFunc(getContainerProgressName(ctr), reason))
	}
	return events
}
