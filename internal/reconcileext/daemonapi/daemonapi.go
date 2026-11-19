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

// Package daemonapi is a wire-compatible copy of the daemon API point of moby
// (github.com/moby/moby/v2/extpoints/daemonapi/v0, commit 53f4b7843b).
//
// The original lives in the moby/moby/v2 module, whose dependency graph is far
// too heavy to import for one tiny contract. The copy keeps what makes it
// interoperable with the daemon: the point ID
// ("org.mobyproject.extension.daemon.api.v0"), the gRPC service name derived
// from it (and its "DaemonAPI" service) and the field numbers of the messages.
// If the daemon changes the contract, this copy must follow.
//
// EXPERIMENTAL: the point is a temporary shortcut in moby until dedicated
// points exist for what extensions need from the engine.
package daemonapi

import (
	"context"

	"github.com/moby/extensions"
)

// DaemonAPI locates the Engine API endpoint the daemon serves for extensions.
type DaemonAPI interface {
	// Get returns the endpoint.
	Get(ctx context.Context, req *GetRequest) (*GetResponse, error)
}

// Point is single-cardinality for the daemon API endpoint.
//
//mobyextgen:service=DaemonAPI
var Point = extensions.DefineSinglePoint[DaemonAPI]("org.mobyproject.extension.daemon.api.v0")

// GetRequest requests the daemon API endpoint.
type GetRequest struct{}

// GetResponse contains the daemon API endpoint.
type GetResponse struct {
	// Host is the endpoint in DOCKER_HOST form, such as unix:///path/api.sock.
	Host string `pb:"1"`
}
