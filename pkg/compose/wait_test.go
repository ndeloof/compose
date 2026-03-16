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
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"

	"github.com/docker/compose/v5/pkg/mocks"
)

func TestWaitDependencies(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	apiClient := mocks.NewMockAPIClient(mockCtrl)
	cli := mocks.NewMockCli(mockCtrl)
	tested, err := NewComposeService(cli)
	assert.NilError(t, err)
	cli.EXPECT().Client().Return(apiClient).AnyTimes()

	t.Run("should skip dependencies with scale 0", func(t *testing.T) {
		dbService := types.ServiceConfig{Name: "db", Scale: intPtr(0)}
		redisService := types.ServiceConfig{Name: "redis", Scale: intPtr(0)}
		project := types.Project{Name: strings.ToLower(testProject), Services: types.Services{
			"db":    dbService,
			"redis": redisService,
		}}
		dependencies := types.DependsOnConfig{
			"db":    {Condition: ServiceConditionRunningOrHealthy},
			"redis": {Condition: ServiceConditionRunningOrHealthy},
		}
		assert.NilError(t, tested.(*composeService).waitDependencies(t.Context(), &project, "", dependencies, nil, 0))
	})
	t.Run("should skip dependencies with condition service_started", func(t *testing.T) {
		dbService := types.ServiceConfig{Name: "db", Scale: intPtr(1)}
		redisService := types.ServiceConfig{Name: "redis", Scale: intPtr(1)}
		project := types.Project{Name: strings.ToLower(testProject), Services: types.Services{
			"db":    dbService,
			"redis": redisService,
		}}
		dependencies := types.DependsOnConfig{
			"db":    {Condition: types.ServiceConditionStarted, Required: true},
			"redis": {Condition: types.ServiceConditionStarted, Required: true},
		}
		assert.NilError(t, tested.(*composeService).waitDependencies(t.Context(), &project, "", dependencies, nil, 0))
	})
}

func TestIsServiceHealthy(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	apiClient := mocks.NewMockAPIClient(mockCtrl)
	cli := mocks.NewMockCli(mockCtrl)
	tested, err := NewComposeService(cli)
	assert.NilError(t, err)
	cli.EXPECT().Client().Return(apiClient).AnyTimes()

	ctx := t.Context()

	t.Run("disabled healthcheck with fallback to running", func(t *testing.T) {
		containerID := "test-container-id"
		containers := Containers{
			{ID: containerID},
		}

		// Container with disabled healthcheck (Test: ["NONE"])
		apiClient.EXPECT().ContainerInspect(ctx, containerID, gomock.Any()).Return(client.ContainerInspectResult{
			Container: container.InspectResponse{
				ID:    containerID,
				Name:  "test-container",
				State: &container.State{Status: "running"},
				Config: &container.Config{
					Healthcheck: &container.HealthConfig{
						Test: []string{"NONE"},
					},
				},
			},
		}, nil)

		isHealthy, err := tested.(*composeService).isServiceHealthy(ctx, containers, true)
		assert.NilError(t, err)
		assert.Equal(t, true, isHealthy, "Container with disabled healthcheck should be considered healthy when running with fallbackRunning=true")
	})

	t.Run("disabled healthcheck without fallback", func(t *testing.T) {
		containerID := "test-container-id"
		containers := Containers{
			{ID: containerID},
		}

		// Container with disabled healthcheck (Test: ["NONE"]) but fallbackRunning=false
		apiClient.EXPECT().ContainerInspect(ctx, containerID, gomock.Any()).Return(client.ContainerInspectResult{
			Container: container.InspectResponse{
				ID:    containerID,
				Name:  "test-container",
				State: &container.State{Status: "running"},
				Config: &container.Config{
					Healthcheck: &container.HealthConfig{
						Test: []string{"NONE"},
					},
				},
			},
		}, nil)

		_, err := tested.(*composeService).isServiceHealthy(ctx, containers, false)
		assert.ErrorContains(t, err, "has no healthcheck configured")
	})

	t.Run("no healthcheck with fallback to running", func(t *testing.T) {
		containerID := "test-container-id"
		containers := Containers{
			{ID: containerID},
		}

		// Container with no healthcheck at all
		apiClient.EXPECT().ContainerInspect(ctx, containerID, gomock.Any()).Return(client.ContainerInspectResult{
			Container: container.InspectResponse{
				ID:    containerID,
				Name:  "test-container",
				State: &container.State{Status: "running"},
				Config: &container.Config{
					Healthcheck: nil,
				},
			},
		}, nil)

		isHealthy, err := tested.(*composeService).isServiceHealthy(ctx, containers, true)
		assert.NilError(t, err)
		assert.Equal(t, true, isHealthy, "Container with no healthcheck should be considered healthy when running with fallbackRunning=true")
	})

	t.Run("exited container with disabled healthcheck", func(t *testing.T) {
		containerID := "test-container-id"
		containers := Containers{
			{ID: containerID},
		}

		// Container with disabled healthcheck but exited
		apiClient.EXPECT().ContainerInspect(ctx, containerID, gomock.Any()).Return(client.ContainerInspectResult{
			Container: container.InspectResponse{
				ID:   containerID,
				Name: "test-container",
				State: &container.State{
					Status:   "exited",
					ExitCode: 1,
				},
				Config: &container.Config{
					Healthcheck: &container.HealthConfig{
						Test: []string{"NONE"},
					},
				},
			},
		}, nil)

		_, err := tested.(*composeService).isServiceHealthy(ctx, containers, true)
		assert.ErrorContains(t, err, "exited")
	})

	t.Run("healthy container with healthcheck", func(t *testing.T) {
		containerID := "test-container-id"
		containers := Containers{
			{ID: containerID},
		}

		// Container with actual healthcheck that is healthy
		apiClient.EXPECT().ContainerInspect(ctx, containerID, gomock.Any()).Return(client.ContainerInspectResult{
			Container: container.InspectResponse{
				ID:   containerID,
				Name: "test-container",
				State: &container.State{
					Status: "running",
					Health: &container.Health{
						Status: container.Healthy,
					},
				},
				Config: &container.Config{
					Healthcheck: &container.HealthConfig{
						Test: []string{"CMD", "curl", "-f", "http://localhost"},
					},
				},
			},
		}, nil)

		isHealthy, err := tested.(*composeService).isServiceHealthy(ctx, containers, false)
		assert.NilError(t, err)
		assert.Equal(t, true, isHealthy, "Container with healthy status should be healthy")
	})
}
