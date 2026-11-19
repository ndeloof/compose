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
	"os"
	"strings"
	"sync"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"

	"github.com/docker/compose/v5/internal/naming"
	"github.com/docker/compose/v5/internal/planning"
	reconcilev0 "github.com/docker/compose/v5/internal/reconcileapi"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
)

// engine is what the reconciler needs from the machinery that acts on the
// daemon. The real one is [composeEngine]; tests substitute a fake.
type engine interface {
	// Up converges the daemon on the project and reports the plan it executed.
	Up(ctx context.Context, project *types.Project, options api.UpOptions) ([]reconcilev0.Step, error)
	// Plan reports what Up would do, without touching anything.
	Plan(ctx context.Context, project *types.Project, options api.UpOptions) ([]reconcilev0.Step, error)
	// Services reports the containers of each service of the project.
	Services(ctx context.Context, project *types.Project) ([]reconcilev0.ServiceState, error)
	// Down removes the resources of a project and reports what it removed.
	Down(ctx context.Context, project string, options api.DownOptions) ([]reconcilev0.Step, error)
}

// composeEngine drives the daemon with pkg/compose, used as a library on the
// Engine API client the daemon.api point provides.
type composeEngine struct {
	api client.APIClient
	cli command.Cli
}

// newComposeEngine builds the Docker CLI facade pkg/compose needs around the
// Engine API client. Nothing of the CLI's own configuration is relied upon:
// the client is given, output goes to stderr (stdout is reserved for the
// extension handshake) and there is no terminal.
func newComposeEngine(apiClient client.APIClient) (*composeEngine, error) {
	cli, err := command.NewDockerCli(
		command.WithAPIClient(apiClient),
		command.WithOutputStream(os.Stderr),
		command.WithErrorStream(os.Stderr),
	)
	if err != nil {
		return nil, err
	}
	if err := cli.Initialize(flags.NewClientOptions()); err != nil {
		return nil, err
	}
	return &composeEngine{api: apiClient, cli: cli}, nil
}

// service builds a Compose service whose plans are collected in *steps.
//
// Images are pulled by the daemon, without any credential of the user: the
// client's registry configuration does not exist here.
func (e *composeEngine) service(dryRun bool, steps *[]reconcilev0.Step) (api.Compose, error) {
	var mu sync.Mutex
	options := []compose.Option{
		compose.WithStreams(os.Stderr, os.Stderr, nil),
		// nobody to answer: keep the default of each question, never destroy
		// something on a guess
		compose.WithPrompt(func(message string, defaultValue bool) (bool, error) {
			logrus.Warnf("%s (no one to ask, answering %t)", message, defaultValue)
			return defaultValue, nil
		}),
		compose.WithPlanObserver(func(plan *planning.Plan) {
			mu.Lock()
			defer mu.Unlock()
			*steps = stepsOf(plan)
		}),
	}
	if dryRun {
		options = append(options, compose.WithDryRun)
	}
	return compose.NewComposeService(e.cli, options...)
}

func (e *composeEngine) Up(ctx context.Context, project *types.Project, options api.UpOptions) ([]reconcilev0.Step, error) {
	return e.up(ctx, false, project, options)
}

func (e *composeEngine) Plan(ctx context.Context, project *types.Project, options api.UpOptions) ([]reconcilev0.Step, error) {
	return e.up(ctx, true, project, options)
}

func (e *composeEngine) up(ctx context.Context, dryRun bool, project *types.Project, options api.UpOptions) ([]reconcilev0.Step, error) {
	var steps []reconcilev0.Step
	svc, err := e.service(dryRun, &steps)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(svc)
	err = svc.Up(ctx, project, options)
	return steps, err
}

func (e *composeEngine) Services(ctx context.Context, project *types.Project) ([]reconcilev0.ServiceState, error) {
	svc, err := e.service(false, new([]reconcilev0.Step))
	if err != nil {
		return nil, err
	}
	defer closeQuietly(svc)
	containers, err := svc.Ps(ctx, project.Name, api.PsOptions{Project: project, All: true})
	if err != nil {
		return nil, err
	}
	return servicesState(project, containers), nil
}

func (e *composeEngine) Down(ctx context.Context, project string, options api.DownOptions) ([]reconcilev0.Step, error) {
	// Down does not report a plan: what it removes is listed beforehand, from
	// the same labels it selects on
	steps, err := e.removalSteps(ctx, project, options.Volumes)
	if err != nil {
		return nil, err
	}
	svc, err := e.service(false, new([]reconcilev0.Step))
	if err != nil {
		return nil, err
	}
	defer closeQuietly(svc)
	if err := svc.Down(ctx, project, options); err != nil {
		return nil, err
	}
	return steps, nil
}

// removalSteps lists what Down will remove, containers first.
func (e *composeEngine) removalSteps(ctx context.Context, project string, volumes bool) ([]reconcilev0.Step, error) {
	filters := naming.ProjectFilter(project)
	var steps []reconcilev0.Step
	containers, err := e.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, err
	}
	for _, c := range containers.Items {
		steps = append(steps, reconcilev0.Step{
			Operation: opRemoveContainer,
			Resource:  containerName(c.Names),
			Service:   c.Labels[api.ServiceLabel],
		})
	}
	networks, err := e.api.NetworkList(ctx, client.NetworkListOptions{Filters: filters})
	if err != nil {
		return nil, err
	}
	for _, n := range networks.Items {
		if _, relay := n.Labels[api.RelayNetworkLabel]; relay && n.Labels[api.NetworkLabel] == "" {
			continue
		}
		steps = append(steps, reconcilev0.Step{Operation: opRemoveNetwork, Resource: n.Name})
	}
	if volumes {
		vols, err := e.api.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
		if err != nil {
			return nil, err
		}
		for _, v := range vols.Items {
			steps = append(steps, reconcilev0.Step{Operation: opRemoveVolume, Resource: v.Name})
		}
	}
	return steps, nil
}

func closeQuietly(svc api.Compose) {
	if c, ok := svc.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}

func containerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

// servicesState groups the containers of a project by service, in the order
// of the model.
func servicesState(project *types.Project, containers []api.ContainerSummary) []reconcilev0.ServiceState {
	byService := map[string]*reconcilev0.ServiceState{}
	var order []string
	for _, name := range project.ServiceNames() {
		byService[name] = &reconcilev0.ServiceState{Name: name}
		order = append(order, name)
	}
	for _, c := range containers {
		st, ok := byService[c.Service]
		if !ok {
			continue
		}
		st.Containers = append(st.Containers, c.ID)
		if c.State == "running" {
			st.Running++
		}
	}
	out := make([]reconcilev0.ServiceState, 0, len(order))
	for _, name := range order {
		out = append(out, *byService[name])
	}
	return out
}

// Operations of a Step. The first twelve are the contract's, the others the
// plan holds besides.
const (
	opCreateNetwork     = "create_network"
	opRemoveNetwork     = "remove_network"
	opConnectNetwork    = "connect_network"
	opDisconnectNetwork = "disconnect_network"
	opCreateVolume      = "create_volume"
	opRemoveVolume      = "remove_volume"
	opCreateContainer   = "create_container"
	opStartContainer    = "start_container"
	opStopContainer     = "stop_container"
	opRemoveContainer   = "remove_container"
	opRenameContainer   = "rename_container"
	opWaitCondition     = "wait_condition"
	opRunProvider       = "run_provider"
	opRunPreStart       = "run_pre_start"
	opRunPostStart      = "run_post_start"
	opCreateHook        = "create_hook_container"
)

var operationNames = map[planning.OperationType]string{
	planning.OpCreateNetwork:       opCreateNetwork,
	planning.OpRemoveNetwork:       opRemoveNetwork,
	planning.OpConnectNetwork:      opConnectNetwork,
	planning.OpDisconnectNetwork:   opDisconnectNetwork,
	planning.OpCreateVolume:        opCreateVolume,
	planning.OpRemoveVolume:        opRemoveVolume,
	planning.OpCreateContainer:     opCreateContainer,
	planning.OpStartContainer:      opStartContainer,
	planning.OpStopContainer:       opStopContainer,
	planning.OpRemoveContainer:     opRemoveContainer,
	planning.OpRenameContainer:     opRenameContainer,
	planning.OpWaitCondition:       opWaitCondition,
	planning.OpRunProvider:         opRunProvider,
	planning.OpRunPreStart:         opRunPreStart,
	planning.OpRunPostStart:        opRunPostStart,
	planning.OpCreateHookContainer: opCreateHook,
}

// stepsOf renders a plan as steps, in the plan's topological order. The
// resource of a step is the NAME of the container, network or volume it
// applies to (the client reports progress by name).
func stepsOf(plan *planning.Plan) []reconcilev0.Step {
	// a container is created under a temporary name when it replaces another:
	// its steps carry the name it ends up with, given by the rename node
	finalName := map[int]string{}
	for _, node := range plan.Nodes {
		if node.Operation.Type == planning.OpRenameContainer {
			finalName[node.Operation.CreateNodeID] = node.Operation.Name
		}
	}
	createName := map[int]string{}
	for _, node := range plan.Nodes {
		if node.Operation.Type == planning.OpCreateContainer {
			name := node.Operation.Name
			if final, ok := finalName[node.ID]; ok {
				name = final
			}
			createName[node.ID] = name
		}
	}

	steps := make([]reconcilev0.Step, 0, len(plan.Nodes))
	for _, node := range plan.Nodes {
		op := node.Operation
		name, ok := operationNames[op.Type]
		if !ok {
			name = strings.ToLower(op.Type.String())
		}
		resource := op.Name
		switch op.Type {
		case planning.OpCreateContainer:
			resource = createName[node.ID]
		case planning.OpStartContainer, planning.OpRunPreStart, planning.OpRunPostStart:
			if n, ok := createName[op.CreateNodeID]; ok {
				resource = n
			} else if op.Container != nil {
				resource = containerName(op.Container.Names)
			}
		case planning.OpStopContainer, planning.OpRemoveContainer:
			if op.Container != nil {
				resource = containerName(op.Container.Names)
			}
		}
		if resource == "" {
			resource = op.ResourceID
		}
		steps = append(steps, reconcilev0.Step{
			Operation: name,
			Resource:  resource,
			Service:   serviceOf(op),
			Reason:    op.Cause,
		})
	}
	return steps
}

// serviceOf is the service an operation belongs to, if it does.
func serviceOf(op planning.Operation) string {
	if op.Service != nil {
		return op.Service.Name
	}
	if op.Container != nil {
		if s := op.Container.Labels[api.ServiceLabel]; s != "" {
			return s
		}
	}
	// "service:web:1"
	if rest, ok := strings.CutPrefix(op.ResourceID, "service:"); ok {
		if i := strings.LastIndex(rest, ":"); i > 0 {
			return rest[:i]
		}
		return rest
	}
	return ""
}
