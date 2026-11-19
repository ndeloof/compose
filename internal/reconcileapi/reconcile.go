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

//go:generate go run github.com/moby/extensions/cmd/mobyextgen

// Package reconcilev0 defines the extension point through which the daemon
// converges the resources it runs (containers, networks, volumes) on a Compose
// application model.
//
// EXPERIMENTAL: the contract is a prototype and carries no compatibility promise.
//
// The client hands over the model already resolved: interpolation, env files,
// extends, include, profiles and relative paths are the client's business, so
// that the extension only has to read a canonical Compose document (what
// `docker compose config` prints). What the client cannot hand over, the
// extension refuses rather than approximates (see Admit).
//
// Wire-format conventions, constrained by the mobyextgen generator (unary
// methods only; no enums, oneof, or well-known types):
//   - enumerations are documented strings;
//   - durations are int64 seconds, zero meaning "the default";
//   - the model travels as a []byte payload (see ApplyRequest.Model).
package reconcilev0

import (
	"context"

	"github.com/moby/extensions"
)

// ExtensionID is the identifier of the extension: the name of its binary in
// the daemon's extension directory.
const ExtensionID = "io.docker.compose.reconcile.v0"

// Reconciler converges the daemon's resources on a Compose model.
//
// Errors are returned as gRPC status codes: InvalidArgument for a model that
// cannot be read or that the extension refuses (the message lists each reason
// with the path it concerns), FailedPrecondition for a convergence that
// failed halfway (the daemon is left as far as it went), Unavailable while the
// extension is starting, and Canceled when the caller went away.
type Reconciler interface {
	// Apply converges the resources of a project on the model: it creates what
	// is missing, recreates what diverged, starts what has to run in the order
	// of the dependencies and, when asked, removes what the model no longer
	// has. It is idempotent: applying the same model twice changes nothing the
	// second time. With DryRun it only reports what it would do.
	Apply(ctx context.Context, req *ApplyRequest) (*ApplyReply, error)

	// Remove stops and removes the resources of a project, containers first,
	// then the networks, and the volumes when asked.
	Remove(ctx context.Context, req *RemoveRequest) (*RemoveReply, error)

	// Admit tells whether the model can be applied as it is, without touching
	// anything. It lists every reason it cannot, each with its path in the
	// model.
	Admit(ctx context.Context, req *AdmitRequest) (*AdmitReply, error)
}

// Point is single-cardinality: there is one authoritative reconciler.
//
//mobyextgen:service=Reconciler
var Point = extensions.DefineSinglePoint[Reconciler]("io.docker.compose.extension.reconcile.api.v0")

// Recreate policies of ApplyRequest.Recreate.
const (
	// RecreateDiverged recreates a container whose configuration, image or
	// dependencies changed. It is the default.
	RecreateDiverged = "diverged"
	// RecreateNever keeps every existing container as it is.
	RecreateNever = "never"
	// RecreateForce recreates every container.
	RecreateForce = "force"
)

// ApplyRequest asks the daemon to converge a project on a model.
type ApplyRequest struct {
	// Project is the name of the project. It scopes the resources the
	// extension manages (its label value). Required.
	Project string `pb:"1"`
	// Model is the canonical, fully resolved Compose document in YAML, as
	// `docker compose config` prints it. Required.
	Model []byte `pb:"2"`
	// Services restricts the convergence to these services and what they
	// depend on. Empty means every service.
	Services []string `pb:"3"`
	// Recreate is the recreate policy, one of the Recreate* constants.
	// Empty means RecreateDiverged.
	Recreate string `pb:"4"`
	// RemoveOrphans removes the containers of the project that no service of
	// the model accounts for.
	RemoveOrphans bool `pb:"5"`
	// Wait holds the call open until the services are running and, for those
	// declaring a healthcheck, healthy.
	Wait bool `pb:"6"`
	// WaitTimeoutSeconds bounds Wait. Zero means no bound other than the
	// caller's context.
	WaitTimeoutSeconds int64 `pb:"7"`
	// DryRun reports the plan without applying it.
	DryRun bool `pb:"8"`
}

// ApplyReply reports what the convergence did, or would do with DryRun.
type ApplyReply struct {
	// Steps are the operations of the plan, in the order of their execution
	// when they depend on one another.
	Steps []Step `pb:"1"`
	// Services is the state of each service once converged. Empty with DryRun.
	Services []ServiceState `pb:"2"`
}

// Step is one operation of a plan.
type Step struct {
	// Operation is one of: create_network, remove_network, connect_network,
	// disconnect_network, create_volume, remove_volume, create_container,
	// start_container, stop_container, remove_container, rename_container,
	// wait_condition.
	Operation string `pb:"1"`
	// Resource is the name of the resource the operation applies to.
	Resource string `pb:"2"`
	// Service is the service the resource belongs to, when it does.
	Service string `pb:"3"`
	// Reason says why, for the operations that replace something: for
	// instance "config hash changed".
	Reason string `pb:"4"`
}

// ServiceState is the state of a service after an Apply.
type ServiceState struct {
	// Name is the service name.
	Name string `pb:"1"`
	// Containers are the IDs of its containers.
	Containers []string `pb:"2"`
	// Running is the number of its containers that run.
	Running uint32 `pb:"3"`
}

// RemoveRequest asks the daemon to remove the resources of a project.
type RemoveRequest struct {
	// Project is the name of the project. Required.
	Project string `pb:"1"`
	// Volumes removes the named volumes of the project too.
	Volumes bool `pb:"2"`
	// RemoveOrphans removes the containers of the project no model accounts
	// for. They are always removed with the project; the flag is kept for
	// symmetry with Apply.
	RemoveOrphans bool `pb:"3"`
	// TimeoutSeconds is the grace period given to a container to stop. Zero
	// means the container's own.
	TimeoutSeconds int64 `pb:"4"`
}

// RemoveReply reports what was removed.
type RemoveReply struct {
	// Steps are the operations performed, as in ApplyReply.
	Steps []Step `pb:"1"`
}

// AdmitRequest asks whether a model can be applied.
type AdmitRequest struct {
	// Project is the name of the project. Required.
	Project string `pb:"1"`
	// Model is the canonical Compose document, as in ApplyRequest.
	Model []byte `pb:"2"`
}

// AdmitReply lists the reasons a model is refused. It is empty when the model
// is admitted.
type AdmitReply struct {
	Refusals []Refusal `pb:"1"`
}

// Refusal is one reason a model cannot be applied.
type Refusal struct {
	// Path locates the offending part of the model, for instance
	// "services.web.build".
	Path string `pb:"1"`
	// Reason says why, in terms the user can act on.
	Reason string `pb:"2"`
}
