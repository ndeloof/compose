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
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"testing"

	composeloader "github.com/compose-spec/compose-go/v2/loader"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/moby/moby/api/types/container"
	mountTypes "github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/mocks"
)

func TestBuildBindMount(t *testing.T) {
	project := composetypes.Project{}
	volume := composetypes.ServiceVolumeConfig{
		Type:   composetypes.VolumeTypeBind,
		Source: "",
		Target: "/data",
	}
	mount, err := buildMount(project, volume)
	assert.NilError(t, err)
	assert.Assert(t, filepath.IsAbs(mount.Source))
	_, err = os.Stat(mount.Source)
	assert.NilError(t, err)
	assert.Equal(t, mount.Type, mountTypes.TypeBind)
}

func TestBuildNamedPipeMount(t *testing.T) {
	project := composetypes.Project{}
	volume := composetypes.ServiceVolumeConfig{
		Type:   composetypes.VolumeTypeNamedPipe,
		Source: "\\\\.\\pipe\\docker_engine_windows",
		Target: "\\\\.\\pipe\\docker_engine",
	}
	mount, err := buildMount(project, volume)
	assert.NilError(t, err)
	assert.Equal(t, mount.Type, mountTypes.TypeNamedPipe)
}

func TestBuildVolumeMount(t *testing.T) {
	project := composetypes.Project{
		Name: "myProject",
		Volumes: composetypes.Volumes(map[string]composetypes.VolumeConfig{
			"myVolume": {
				Name: "myProject_myVolume",
			},
		}),
	}
	volume := composetypes.ServiceVolumeConfig{
		Type:   composetypes.VolumeTypeVolume,
		Source: "myVolume",
		Target: "/data",
	}
	mount, err := buildMount(project, volume)
	assert.NilError(t, err)
	assert.Equal(t, mount.Source, "myProject_myVolume")
	assert.Equal(t, mount.Type, mountTypes.TypeVolume)
}

func TestServiceImageName(t *testing.T) {
	assert.Equal(t, api.GetImageNameOrDefault(composetypes.ServiceConfig{Image: "myImage"}, "myProject"), "myImage")
	assert.Equal(t, api.GetImageNameOrDefault(composetypes.ServiceConfig{Name: "aService"}, "myProject"), "myProject-aService")
}

func TestPrepareNetworkLabels(t *testing.T) {
	project := composetypes.Project{
		Name:     "myProject",
		Networks: composetypes.Networks(map[string]composetypes.NetworkConfig{"skynet": {}}),
	}
	prepareNetworks(&project)
	assert.DeepEqual(t, project.Networks["skynet"].CustomLabels, composetypes.Labels(map[string]string{
		"com.docker.compose.network": "skynet",
		"com.docker.compose.project": "myProject",
		"com.docker.compose.version": api.ComposeVersion,
	}))
}

func TestBuildContainerMountOptions(t *testing.T) {
	project := composetypes.Project{
		Name: "myProject",
		Services: composetypes.Services{
			"myService": {
				Name: "myService",
				Volumes: []composetypes.ServiceVolumeConfig{
					{
						Type:   composetypes.VolumeTypeVolume,
						Target: "/var/myvolume1",
					},
					{
						Type:   composetypes.VolumeTypeVolume,
						Target: "/var/myvolume2",
					},
					{
						Type:   composetypes.VolumeTypeVolume,
						Source: "myVolume3",
						Target: "/var/myvolume3",
						Volume: &composetypes.ServiceVolumeVolume{
							Subpath: "etc",
						},
					},
					{
						Type:   composetypes.VolumeTypeNamedPipe,
						Source: "\\\\.\\pipe\\docker_engine_windows",
						Target: "\\\\.\\pipe\\docker_engine",
					},
				},
			},
		},
		Volumes: composetypes.Volumes(map[string]composetypes.VolumeConfig{
			"myVolume1": {
				Name: "myProject_myVolume1",
			},
			"myVolume2": {
				Name: "myProject_myVolume2",
			},
		}),
	}

	inherit := &container.Summary{
		Mounts: []container.MountPoint{
			{
				Type:        composetypes.VolumeTypeVolume,
				Destination: "/var/myvolume1",
			},
			{
				Type:        composetypes.VolumeTypeVolume,
				Destination: "/var/myvolume2",
			},
		},
	}

	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	mock, cli := prepareMocks(mockCtrl)
	s := composeService{
		dockerCli: cli,
	}
	mock.EXPECT().ImageInspect(gomock.Any(), "myProject-myService").AnyTimes().Return(client.ImageInspectResult{}, nil)

	mounts, err := s.buildContainerMountOptions(t.Context(), project, project.Services["myService"], inherit)
	sort.Slice(mounts, func(i, j int) bool {
		return mounts[i].Target < mounts[j].Target
	})
	assert.NilError(t, err)
	assert.Assert(t, len(mounts) == 4)
	assert.Equal(t, mounts[0].Target, "/var/myvolume1")
	assert.Equal(t, mounts[1].Target, "/var/myvolume2")
	assert.Equal(t, mounts[2].Target, "/var/myvolume3")
	assert.Equal(t, mounts[2].VolumeOptions.Subpath, "etc")
	assert.Equal(t, mounts[3].Target, "\\\\.\\pipe\\docker_engine")

	mounts, err = s.buildContainerMountOptions(t.Context(), project, project.Services["myService"], inherit)
	sort.Slice(mounts, func(i, j int) bool {
		return mounts[i].Target < mounts[j].Target
	})
	assert.NilError(t, err)
	assert.Assert(t, len(mounts) == 4)
	assert.Equal(t, mounts[0].Target, "/var/myvolume1")
	assert.Equal(t, mounts[1].Target, "/var/myvolume2")
	assert.Equal(t, mounts[2].Target, "/var/myvolume3")
	assert.Equal(t, mounts[2].VolumeOptions.Subpath, "etc")
	assert.Equal(t, mounts[3].Target, "\\\\.\\pipe\\docker_engine")
}

func TestDefaultNetworkSettings(t *testing.T) {
	t.Run("returns the network with the highest priority as primary when service has multiple networks", func(t *testing.T) {
		service := composetypes.ServiceConfig{
			Name: "myService",
			Networks: map[string]*composetypes.ServiceNetworkConfig{
				"myNetwork1": {
					Priority: 10,
				},
				"myNetwork2": {
					Priority: 1000,
				},
			},
		}
		project := composetypes.Project{
			Name: "myProject",
			Services: composetypes.Services{
				"myService": service,
			},
			Networks: composetypes.Networks(map[string]composetypes.NetworkConfig{
				"myNetwork1": {
					Name: "myProject_myNetwork1",
				},
				"myNetwork2": {
					Name: "myProject_myNetwork2",
				},
			}),
		}

		networkMode, networkConfig, err := defaultNetworkSettings(&project, service, 1, nil, true, "1.44")
		assert.NilError(t, err)
		assert.Equal(t, string(networkMode), "myProject_myNetwork2")
		assert.Check(t, cmp.Len(networkConfig.EndpointsConfig, 2))
		assert.Check(t, cmp.Contains(networkConfig.EndpointsConfig, "myProject_myNetwork1"))
		assert.Check(t, cmp.Contains(networkConfig.EndpointsConfig, "myProject_myNetwork2"))
	})

	t.Run("returns default network when service has no networks", func(t *testing.T) {
		service := composetypes.ServiceConfig{
			Name: "myService",
		}
		project := composetypes.Project{
			Name: "myProject",
			Services: composetypes.Services{
				"myService": service,
			},
			Networks: composetypes.Networks(map[string]composetypes.NetworkConfig{
				"myNetwork1": {
					Name: "myProject_myNetwork1",
				},
				"myNetwork2": {
					Name: "myProject_myNetwork2",
				},
				"default": {
					Name: "myProject_default",
				},
			}),
		}

		networkMode, networkConfig, err := defaultNetworkSettings(&project, service, 1, nil, true, "1.44")
		assert.NilError(t, err)
		assert.Equal(t, string(networkMode), "myProject_default")
		assert.Check(t, cmp.Len(networkConfig.EndpointsConfig, 1))
		assert.Check(t, cmp.Contains(networkConfig.EndpointsConfig, "myProject_default"))
	})

	t.Run("returns none if project has no networks", func(t *testing.T) {
		service := composetypes.ServiceConfig{
			Name: "myService",
		}
		project := composetypes.Project{
			Name: "myProject",
			Services: composetypes.Services{
				"myService": service,
			},
		}

		networkMode, networkConfig, err := defaultNetworkSettings(&project, service, 1, nil, true, "1.44")
		assert.NilError(t, err)
		assert.Equal(t, string(networkMode), "none")
		assert.Check(t, cmp.Nil(networkConfig))
	})

	t.Run("returns defined network mode if explicitly set", func(t *testing.T) {
		service := composetypes.ServiceConfig{
			Name:        "myService",
			NetworkMode: "host",
		}
		project := composetypes.Project{
			Name:     "myProject",
			Services: composetypes.Services{"myService": service},
			Networks: composetypes.Networks(map[string]composetypes.NetworkConfig{
				"default": {
					Name: "myProject_default",
				},
			}),
		}

		networkMode, networkConfig, err := defaultNetworkSettings(&project, service, 1, nil, true, "1.44")
		assert.NilError(t, err)
		assert.Equal(t, string(networkMode), "host")
		assert.Check(t, cmp.Nil(networkConfig))
	})
}

func TestCreateEndpointSettings(t *testing.T) {
	eps, err := createEndpointSettings(&composetypes.Project{
		Name: "projName",
	}, composetypes.ServiceConfig{
		Name:          "serviceName",
		ContainerName: "containerName",
		Networks: map[string]*composetypes.ServiceNetworkConfig{
			"netName": {
				Priority:     100,
				Aliases:      []string{"alias1", "alias2"},
				Ipv4Address:  "10.16.17.18",
				Ipv6Address:  "fdb4:7a7f:373a:3f0c::42",
				LinkLocalIPs: []string{"169.254.10.20"},
				MacAddress:   "02:00:00:00:00:01",
				DriverOpts: composetypes.Options{
					"driverOpt1": "optval1",
					"driverOpt2": "optval2",
				},
			},
		},
	}, 0, "netName", []string{"link1", "link2"}, true)
	assert.NilError(t, err)
	macAddr, _ := net.ParseMAC("02:00:00:00:00:01")
	assert.Check(t, cmp.DeepEqual(eps, &network.EndpointSettings{
		IPAMConfig: &network.EndpointIPAMConfig{
			IPv4Address:  netip.MustParseAddr("10.16.17.18").Unmap(),
			IPv6Address:  netip.MustParseAddr("fdb4:7a7f:373a:3f0c::42"),
			LinkLocalIPs: []netip.Addr{netip.MustParseAddr("169.254.10.20").Unmap()},
		},
		Links:      []string{"link1", "link2"},
		Aliases:    []string{"containerName", "serviceName", "alias1", "alias2"},
		MacAddress: network.HardwareAddr(macAddr),
		DriverOpts: map[string]string{
			"driverOpt1": "optval1",
			"driverOpt2": "optval2",
		},

		// FIXME(robmry) - IPAddress and IPv6Gateway are "operational data" fields...
		//  - The IPv6 address here is the container's address, not the gateway.
		//  - Both fields will be cleared by the daemon, but they could be removed from
		//    the request.
		IPAddress:   netip.MustParseAddr("10.16.17.18").Unmap(),
		IPv6Gateway: netip.MustParseAddr("fdb4:7a7f:373a:3f0c::42"),
	}, cmpopts.EquateComparable(netip.Addr{})))
}

func Test_buildContainerVolumes(t *testing.T) {
	pwd, err := os.Getwd()
	assert.NilError(t, err)

	tests := []struct {
		name   string
		yaml   string
		binds  []string
		mounts []mountTypes.Mount
	}{
		{
			name: "bind mount local path",
			yaml: `
services:
  test:
    volumes:
      - ./data:/data
`,
			binds:  []string{filepath.Join(pwd, "data") + ":/data:rw"},
			mounts: nil,
		},
		{
			name: "bind mount, not create host path",
			yaml: `
services:
  test:
    volumes:
      - type: bind
        source: ./data
        target: /data
        bind:
          create_host_path: false
`,
			binds: nil,
			mounts: []mountTypes.Mount{
				{
					Type:        "bind",
					Source:      filepath.Join(pwd, "data"),
					Target:      "/data",
					BindOptions: &mountTypes.BindOptions{CreateMountpoint: false},
				},
			},
		},
		{
			name: "mount volume",
			yaml: `
services:
  test:
    volumes:
      - data:/data
volumes:
  data:
    name: my_volume
`,
			binds:  []string{"my_volume:/data:rw"},
			mounts: nil,
		},
		{
			name: "mount volume, readonly",
			yaml: `
services:
  test:
    volumes:
      - data:/data:ro
volumes:
  data:
    name: my_volume
`,
			binds:  []string{"my_volume:/data:ro"},
			mounts: nil,
		},
		{
			name: "mount volume subpath",
			yaml: `
services:
  test:
    volumes:
      - type: volume
        source: data
        target: /data
        volume:
          subpath: test/
volumes:
  data: 
    name: my_volume
`,
			binds: nil,
			mounts: []mountTypes.Mount{
				{
					Type:          "volume",
					Source:        "my_volume",
					Target:        "/data",
					VolumeOptions: &mountTypes.VolumeOptions{Subpath: "test/"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := composeloader.LoadWithContext(t.Context(), composetypes.ConfigDetails{
				ConfigFiles: []composetypes.ConfigFile{
					{
						Filename: "test",
						Content:  []byte(tt.yaml),
					},
				},
			}, func(options *composeloader.Options) {
				options.SkipValidation = true
				options.SkipConsistencyCheck = true
			})
			assert.NilError(t, err)
			s := &composeService{}
			binds, mounts, err := s.buildContainerVolumes(t.Context(), *p, p.Services["test"], nil)
			assert.NilError(t, err)
			assert.DeepEqual(t, tt.binds, binds)
			assert.DeepEqual(t, tt.mounts, mounts)
		})
	}
}

func TestContainerName(t *testing.T) {
	s := composetypes.ServiceConfig{
		Name:          "testservicename",
		ContainerName: "testcontainername",
		Scale:         intPtr(1),
		Deploy:        &composetypes.DeployConfig{},
	}
	ret, err := getScale(s)
	assert.NilError(t, err)
	assert.Equal(t, ret, *s.Scale)

	s.Scale = intPtr(0)
	ret, err = getScale(s)
	assert.NilError(t, err)
	assert.Equal(t, ret, *s.Scale)

	s.Scale = intPtr(2)
	_, err = getScale(s)
	assert.Error(t, err, fmt.Sprintf(doubledContainerNameWarning, s.Name, s.ContainerName))
}

func intPtr(i int) *int {
	return &i
}

func TestServiceLinks(t *testing.T) {
	const dbContainerName = "/" + testProject + "-db-1"
	const webContainerName = "/" + testProject + "-web-1"
	s := composetypes.ServiceConfig{
		Name:  "web",
		Scale: intPtr(1),
	}

	containerListOptions := client.ContainerListOptions{
		Filters: projectFilter(testProject).Add("label",
			serviceFilter("db"),
			oneOffFilter(false),
			hasConfigHashLabel(),
		),
		All: true,
	}

	t.Run("service links default", func(t *testing.T) {
		mockCtrl := gomock.NewController(t)
		defer mockCtrl.Finish()

		apiClient := mocks.NewMockAPIClient(mockCtrl)
		cli := mocks.NewMockCli(mockCtrl)
		tested, err := NewComposeService(cli)
		assert.NilError(t, err)
		cli.EXPECT().Client().Return(apiClient).AnyTimes()

		s.Links = []string{"db"}

		c := testContainer("db", dbContainerName, false)
		apiClient.EXPECT().ContainerList(gomock.Any(), containerListOptions).Return(client.ContainerListResult{
			Items: []container.Summary{c},
		}, nil)

		links, err := tested.(*composeService).getLinks(t.Context(), testProject, s, 1)
		assert.NilError(t, err)

		assert.Equal(t, len(links), 3)
		assert.Equal(t, links[0], "testProject-db-1:db")
		assert.Equal(t, links[1], "testProject-db-1:db-1")
		assert.Equal(t, links[2], "testProject-db-1:testProject-db-1")
	})

	t.Run("service links", func(t *testing.T) {
		mockCtrl := gomock.NewController(t)
		defer mockCtrl.Finish()
		apiClient := mocks.NewMockAPIClient(mockCtrl)
		cli := mocks.NewMockCli(mockCtrl)
		tested, err := NewComposeService(cli)
		assert.NilError(t, err)
		cli.EXPECT().Client().Return(apiClient).AnyTimes()

		s.Links = []string{"db:db"}

		c := testContainer("db", dbContainerName, false)

		apiClient.EXPECT().ContainerList(gomock.Any(), containerListOptions).Return(client.ContainerListResult{
			Items: []container.Summary{c},
		}, nil)
		links, err := tested.(*composeService).getLinks(t.Context(), testProject, s, 1)
		assert.NilError(t, err)

		assert.Equal(t, len(links), 3)
		assert.Equal(t, links[0], "testProject-db-1:db")
		assert.Equal(t, links[1], "testProject-db-1:db-1")
		assert.Equal(t, links[2], "testProject-db-1:testProject-db-1")
	})

	t.Run("service links name", func(t *testing.T) {
		mockCtrl := gomock.NewController(t)
		defer mockCtrl.Finish()
		apiClient := mocks.NewMockAPIClient(mockCtrl)
		cli := mocks.NewMockCli(mockCtrl)
		tested, err := NewComposeService(cli)
		assert.NilError(t, err)
		cli.EXPECT().Client().Return(apiClient).AnyTimes()

		s.Links = []string{"db:dbname"}

		c := testContainer("db", dbContainerName, false)
		apiClient.EXPECT().ContainerList(gomock.Any(), containerListOptions).Return(client.ContainerListResult{
			Items: []container.Summary{c},
		}, nil)

		links, err := tested.(*composeService).getLinks(t.Context(), testProject, s, 1)
		assert.NilError(t, err)

		assert.Equal(t, len(links), 3)
		assert.Equal(t, links[0], "testProject-db-1:dbname")
		assert.Equal(t, links[1], "testProject-db-1:db-1")
		assert.Equal(t, links[2], "testProject-db-1:testProject-db-1")
	})

	t.Run("service links external links", func(t *testing.T) {
		mockCtrl := gomock.NewController(t)
		defer mockCtrl.Finish()
		apiClient := mocks.NewMockAPIClient(mockCtrl)
		cli := mocks.NewMockCli(mockCtrl)
		tested, err := NewComposeService(cli)
		assert.NilError(t, err)
		cli.EXPECT().Client().Return(apiClient).AnyTimes()

		s.Links = []string{"db:dbname"}
		s.ExternalLinks = []string{"db1:db2"}

		c := testContainer("db", dbContainerName, false)
		apiClient.EXPECT().ContainerList(gomock.Any(), containerListOptions).Return(client.ContainerListResult{
			Items: []container.Summary{c},
		}, nil)

		links, err := tested.(*composeService).getLinks(t.Context(), testProject, s, 1)
		assert.NilError(t, err)

		assert.Equal(t, len(links), 4)
		assert.Equal(t, links[0], "testProject-db-1:dbname")
		assert.Equal(t, links[1], "testProject-db-1:db-1")
		assert.Equal(t, links[2], "testProject-db-1:testProject-db-1")

		// ExternalLink
		assert.Equal(t, links[3], "db1:db2")
	})

	t.Run("service links itself oneoff", func(t *testing.T) {
		mockCtrl := gomock.NewController(t)
		defer mockCtrl.Finish()
		apiClient := mocks.NewMockAPIClient(mockCtrl)
		cli := mocks.NewMockCli(mockCtrl)
		tested, err := NewComposeService(cli)
		assert.NilError(t, err)
		cli.EXPECT().Client().Return(apiClient).AnyTimes()

		s.Links = []string{}
		s.ExternalLinks = []string{}
		s.Labels = s.Labels.Add(api.OneoffLabel, "True")

		c := testContainer("web", webContainerName, true)
		containerListOptionsOneOff := client.ContainerListOptions{
			Filters: projectFilter(testProject).Add("label",
				serviceFilter("web"),
				oneOffFilter(false),
				hasConfigHashLabel(),
			),
			All: true,
		}
		apiClient.EXPECT().ContainerList(gomock.Any(), containerListOptionsOneOff).Return(client.ContainerListResult{
			Items: []container.Summary{c},
		}, nil)

		links, err := tested.(*composeService).getLinks(t.Context(), testProject, s, 1)
		assert.NilError(t, err)

		assert.Equal(t, len(links), 3)
		assert.Equal(t, links[0], "testProject-web-1:web")
		assert.Equal(t, links[1], "testProject-web-1:web-1")
		assert.Equal(t, links[2], "testProject-web-1:testProject-web-1")
	})
}

func TestCreateMobyContainer(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()
	apiClient := mocks.NewMockAPIClient(mockCtrl)
	cli := mocks.NewMockCli(mockCtrl)
	tested, err := NewComposeService(cli)
	assert.NilError(t, err)
	cli.EXPECT().Client().Return(apiClient).AnyTimes()
	cli.EXPECT().ConfigFile().Return(&configfile.ConfigFile{}).AnyTimes()
	apiClient.EXPECT().DaemonHost().Return("").AnyTimes()
	apiClient.EXPECT().ImageInspect(anyCancellableContext(), gomock.Any()).Return(client.ImageInspectResult{}, nil).AnyTimes()

	// force `RuntimeVersion` to fetch fresh version
	runtimeVersion = runtimeVersionCache{}
	apiClient.EXPECT().ServerVersion(gomock.Any(), gomock.Any()).Return(client.ServerVersionResult{
		APIVersion: "1.44",
	}, nil).AnyTimes()

	service := composetypes.ServiceConfig{
		Name: "test",
		Networks: map[string]*composetypes.ServiceNetworkConfig{
			"a": {
				Priority: 10,
			},
			"b": {
				Priority: 100,
			},
		},
	}
	project := composetypes.Project{
		Name: "bork",
		Services: composetypes.Services{
			"test": service,
		},
		Networks: composetypes.Networks{
			"a": composetypes.NetworkConfig{
				Name: "a-moby-name",
			},
			"b": composetypes.NetworkConfig{
				Name: "b-moby-name",
			},
		},
	}

	var got client.ContainerCreateOptions
	apiClient.EXPECT().ContainerCreate(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
		got = opts
		return client.ContainerCreateResult{ID: "an-id"}, nil
	})

	apiClient.EXPECT().ContainerInspect(gomock.Any(), gomock.Eq("an-id"), gomock.Any()).Times(1).Return(client.ContainerInspectResult{
		Container: container.InspectResponse{
			ID:              "an-id",
			Name:            "a-name",
			Config:          &container.Config{},
			NetworkSettings: &container.NetworkSettings{},
		},
	}, nil)

	_, err = tested.(*composeService).createMobyContainer(t.Context(), &project, service, "test", 0, nil, createOptions{
		Labels: make(composetypes.Labels),
	})
	var falseBool bool
	want := client.ContainerCreateOptions{
		Config: &container.Config{
			AttachStdout: true,
			AttachStderr: true,
			Image:        "bork-test",
			Labels: map[string]string{
				"com.docker.compose.config-hash": "8dbce408396f8986266bc5deba0c09cfebac63c95c2238e405c7bee5f1bd84b8",
				"com.docker.compose.depends_on":  "",
			},
		},
		HostConfig: &container.HostConfig{
			PortBindings: network.PortMap{},
			ExtraHosts:   []string{},
			Tmpfs:        map[string]string{},
			Resources: container.Resources{
				OomKillDisable: &falseBool,
			},
			NetworkMode: "b-moby-name",
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				"a-moby-name": {
					IPAMConfig: &network.EndpointIPAMConfig{},
					Aliases:    []string{"bork-test-0"},
				},
				"b-moby-name": {
					IPAMConfig: &network.EndpointIPAMConfig{},
					Aliases:    []string{"bork-test-0"},
				},
			},
		},
		Name: "test",
	}
	assert.DeepEqual(t, want, got, cmpopts.EquateComparable(netip.Addr{}), cmpopts.EquateEmpty())
	assert.NilError(t, err)
}
