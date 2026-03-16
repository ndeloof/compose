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
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

	"github.com/docker/compose/v5/pkg/api"
)

func (s *composeService) Wait(ctx context.Context, projectName string, options api.WaitOptions) (int64, error) {
	containers, err := s.getContainers(ctx, projectName, oneOffInclude, false, options.Services...)
	if err != nil {
		return 0, err
	}
	if len(containers) == 0 {
		return 0, fmt.Errorf("no containers for project %q", projectName)
	}

	eg, waitCtx := errgroup.WithContext(ctx)
	var statusCode int64
	for _, ctr := range containers {
		eg.Go(func() error {
			var err error
			res := s.apiClient().ContainerWait(waitCtx, ctr.ID, client.ContainerWaitOptions{})
			select {
			case result := <-res.Result:
				_, _ = fmt.Fprintf(s.stdout(), "container %q exited with status code %d\n", ctr.ID, result.StatusCode)
				statusCode = result.StatusCode
			case err = <-res.Error:
			}
			return err
		})
	}

	err = eg.Wait()
	if err != nil {
		return 42, err // Ignore abort flag in case of error in wait
	}

	if options.DownProjectOnContainerExit {
		return statusCode, s.Down(ctx, projectName, api.DownOptions{
			RemoveOrphans: true,
		})
	}

	return statusCode, err
}

// ServiceConditionRunningOrHealthy is a service condition on status running or healthy
const ServiceConditionRunningOrHealthy = "running_or_healthy"

//nolint:gocyclo
func (s *composeService) waitDependencies(ctx context.Context, project *types.Project, dependant string, dependencies types.DependsOnConfig, containers Containers, timeout time.Duration) error {
	if timeout > 0 {
		withTimeout, cancelFunc := context.WithTimeout(ctx, timeout)
		defer cancelFunc()
		ctx = withTimeout
	}
	eg, ctx := errgroup.WithContext(ctx)
	for dep, config := range dependencies {
		if shouldWait, err := shouldWaitForDependency(dep, config, project); err != nil {
			return err
		} else if !shouldWait {
			continue
		}

		waitingFor := containers.filter(isService(dep), isNotOneOff)
		s.events.On(containerEvents(waitingFor, waiting)...)
		if len(waitingFor) == 0 {
			if config.Required {
				return fmt.Errorf("%s is missing dependency %s", dependant, dep)
			}
			logrus.Warnf("%s is missing dependency %s", dependant, dep)
			continue
		}

		eg.Go(func() error {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
				case <-ctx.Done():
					return nil
				}
				switch config.Condition {
				case ServiceConditionRunningOrHealthy:
					isHealthy, err := s.isServiceHealthy(ctx, waitingFor, true)
					if err != nil {
						if !config.Required {
							s.events.On(containerReasonEvents(waitingFor, skippedEvent,
								fmt.Sprintf("optional dependency %q is not running or is unhealthy", dep))...)
							logrus.Warnf("optional dependency %q is not running or is unhealthy: %s", dep, err.Error())
							return nil
						}
						return err
					}
					if isHealthy {
						s.events.On(containerEvents(waitingFor, healthy)...)
						return nil
					}
				case types.ServiceConditionHealthy:
					isHealthy, err := s.isServiceHealthy(ctx, waitingFor, false)
					if err != nil {
						if !config.Required {
							s.events.On(containerReasonEvents(waitingFor, skippedEvent,
								fmt.Sprintf("optional dependency %q failed to start", dep))...)
							logrus.Warnf("optional dependency %q failed to start: %s", dep, err.Error())
							return nil
						}
						s.events.On(containerEvents(waitingFor, func(s string) api.Resource {
							return errorEventf(s, "dependency %s failed to start", dep)
						})...)
						return fmt.Errorf("dependency failed to start: %w", err)
					}
					if isHealthy {
						s.events.On(containerEvents(waitingFor, healthy)...)
						return nil
					}
				case types.ServiceConditionCompletedSuccessfully:
					isExited, code, err := s.isServiceCompleted(ctx, waitingFor)
					if err != nil {
						return err
					}
					if isExited {
						if code == 0 {
							s.events.On(containerEvents(waitingFor, exited)...)
							return nil
						}

						messageSuffix := fmt.Sprintf("%q didn't complete successfully: exit %d", dep, code)
						if !config.Required {
							// optional -> mark as skipped & don't propagate error
							s.events.On(containerReasonEvents(waitingFor, skippedEvent,
								fmt.Sprintf("optional dependency %s", messageSuffix))...)
							logrus.Warnf("optional dependency %s", messageSuffix)
							return nil
						}

						msg := fmt.Sprintf("service %s", messageSuffix)
						s.events.On(containerEvents(waitingFor, func(s string) api.Resource {
							return errorEventf(s, "service %s", messageSuffix)
						})...)
						return errors.New(msg)
					}
				default:
					logrus.Warnf("unsupported depends_on condition: %s", config.Condition)
					return nil
				}
			}
		})
	}
	err := eg.Wait()
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("timeout waiting for dependencies")
	}
	return err
}

func shouldWaitForDependency(serviceName string, dependencyConfig types.ServiceDependency, project *types.Project) (bool, error) {
	if dependencyConfig.Condition == types.ServiceConditionStarted {
		// already managed by InDependencyOrder
		return false, nil
	}
	if service, err := project.GetService(serviceName); err != nil {
		for _, ds := range project.DisabledServices {
			if ds.Name == serviceName {
				// don't wait for disabled service (--no-deps)
				return false, nil
			}
		}
		return false, err
	} else if service.GetScale() == 0 {
		// don't wait for the dependency which configured to have 0 containers running
		return false, nil
	} else if service.Provider != nil {
		// don't wait for provider services
		return false, nil
	}
	return true, nil
}

func (s *composeService) isServiceHealthy(ctx context.Context, containers Containers, fallbackRunning bool) (bool, error) {
	for _, c := range containers {
		res, err := s.apiClient().ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			return false, err
		}
		ctr := res.Container
		name := ctr.Name[1:]

		if ctr.State.Status == container.StateExited {
			return false, fmt.Errorf("container %s exited (%d)", name, ctr.State.ExitCode)
		}

		noHealthcheck := ctr.Config.Healthcheck == nil || (len(ctr.Config.Healthcheck.Test) > 0 && ctr.Config.Healthcheck.Test[0] == "NONE")
		if noHealthcheck && fallbackRunning {
			// Container does not define a health check, but we can fall back to "running" state
			return ctr.State != nil && ctr.State.Status == container.StateRunning, nil
		}

		if ctr.State == nil || ctr.State.Health == nil {
			return false, fmt.Errorf("container %s has no healthcheck configured", name)
		}
		switch ctr.State.Health.Status {
		case container.Healthy:
			// Continue by checking the next container.
		case container.Unhealthy:
			return false, fmt.Errorf("container %s is unhealthy", name)
		case container.Starting:
			return false, nil
		default:
			return false, fmt.Errorf("container %s had unexpected health status %q", name, ctr.State.Health.Status)
		}
	}
	return true, nil
}

func (s *composeService) isServiceCompleted(ctx context.Context, containers Containers) (bool, int, error) {
	for _, c := range containers {
		res, err := s.apiClient().ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			return false, 0, err
		}
		if res.Container.State != nil && res.Container.State.Status == container.StateExited {
			return true, res.Container.State.ExitCode, nil
		}
	}
	return false, 0, nil
}
