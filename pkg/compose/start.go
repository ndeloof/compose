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
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/docker/compose/v5/pkg/api"
)

func (s *composeService) Start(ctx context.Context, projectName string, options api.StartOptions) error {
	return Run(ctx, func(ctx context.Context) error {
		return s.start(ctx, strings.ToLower(projectName), options, nil)
	}, "start", s.events)
}

func (s *composeService) start(ctx context.Context, projectName string, options api.StartOptions, listener api.ContainerEventListener) error {
	project := options.Project
	if project == nil {
		var containers Containers
		containers, err := s.getContainers(ctx, projectName, oneOffExclude, true)
		if err != nil {
			return err
		}

		project, err = s.projectFromName(containers, projectName, options.AttachTo...)
		if err != nil {
			return err
		}
	}

	res, err := s.apiClient().ContainerList(ctx, client.ContainerListOptions{
		Filters: projectFilter(project.Name).Add("label", oneOffFilter(false)),
		All:     true,
	})
	if err != nil {
		return err
	}
	containers := Containers(res.Items)

	err = InDependencyOrder(ctx, project, func(c context.Context, name string) error {
		service, err := project.GetService(name)
		if err != nil {
			return err
		}

		return s.startService(ctx, project, service, containers, listener, options.WaitTimeout)
	})
	if err != nil {
		return err
	}

	if options.Wait {
		depends := types.DependsOnConfig{}
		for _, s := range project.Services {
			depends[s.Name] = types.ServiceDependency{
				Condition: getDependencyCondition(s, project),
				Required:  true,
			}
		}
		if options.WaitTimeout > 0 {
			withTimeout, cancel := context.WithTimeout(ctx, options.WaitTimeout)
			ctx = withTimeout
			defer cancel()
		}

		err = s.waitDependencies(ctx, project, project.Name, depends, containers, 0)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("application not healthy after %s", options.WaitTimeout)
			}
			return err
		}
	}

	return nil
}

// getDependencyCondition checks if service is depended on by other services
// with service_completed_successfully condition, and applies that condition
// instead, or --wait will never finish waiting for one-shot containers
func getDependencyCondition(service types.ServiceConfig, project *types.Project) string {
	for _, services := range project.Services {
		for dependencyService, dependencyConfig := range services.DependsOn {
			if dependencyService == service.Name && dependencyConfig.Condition == types.ServiceConditionCompletedSuccessfully {
				return types.ServiceConditionCompletedSuccessfully
			}
		}
	}
	return ServiceConditionRunningOrHealthy
}

// force sequential calls to ContainerStart to prevent race condition in engine assigning ports from ranges
var startMx sync.Mutex

func (s *composeService) startContainer(ctx context.Context, ctr container.Summary) error {
	s.events.On(newEvent(getContainerProgressName(ctr), api.Working, "Restart"))
	startMx.Lock()
	defer startMx.Unlock()
	_, err := s.apiClient().ContainerStart(ctx, ctr.ID, client.ContainerStartOptions{})
	if err != nil {
		return err
	}
	s.events.On(newEvent(getContainerProgressName(ctr), api.Done, "Restarted"))
	return nil
}

func (s *composeService) startService(ctx context.Context,
	project *types.Project, service types.ServiceConfig,
	containers Containers, listener api.ContainerEventListener,
	timeout time.Duration,
) error {
	if service.Deploy != nil && service.Deploy.Replicas != nil && *service.Deploy.Replicas == 0 {
		return nil
	}

	err := s.waitDependencies(ctx, project, service.Name, service.DependsOn, containers, timeout)
	if err != nil {
		return err
	}

	if len(containers) == 0 {
		if service.GetScale() == 0 {
			return nil
		}
		return fmt.Errorf("service %q has no container to start", service.Name)
	}

	for _, ctr := range containers.filter(isService(service.Name)) {
		if ctr.State == container.StateRunning {
			continue
		}

		err = s.injectSecrets(ctx, project, service, ctr.ID)
		if err != nil {
			return err
		}

		err = s.injectConfigs(ctx, project, service, ctr.ID)
		if err != nil {
			return err
		}

		eventName := getContainerProgressName(ctr)
		s.events.On(startingEvent(eventName))
		_, err = s.apiClient().ContainerStart(ctx, ctr.ID, client.ContainerStartOptions{})
		if err != nil {
			return err
		}

		for _, hook := range service.PostStart {
			err = s.runHook(ctx, ctr, service, hook, listener)
			if err != nil {
				return err
			}
		}

		s.events.On(startedEvent(eventName))
	}
	return nil
}
