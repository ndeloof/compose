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

// Package servicegrpc is a copy of the service.grpc point of moby
// (github.com/moby/moby/v2/extpoints/servicegrpc/v0, commit 53f4b7843b), kept
// for the same reason as the daemonapi copy: the moby/moby/v2 module is too
// heavy to import for it.
//
// The point is how an out-of-process extension publishes its own gRPC services
// on docker.sock: the daemon only proxies, by service name, the services
// registered through providers of this point (a point merely offered with
// servicev0.Offer is registered but not routed). The point ID is the interop
// contract.
package servicegrpc

import (
	"github.com/moby/extensions"
	"github.com/moby/extensions/serverpoint"
	"google.golang.org/grpc"
)

// Provider opts an extension onto the daemon socket.
type Provider interface {
	// RegisterServices registers the extension's services on r.
	RegisterServices(r grpc.ServiceRegistrar)
}

// Point is the socket-exposure point.
var Point = extensions.DefinePoint[Provider]("org.mobyproject.extension.service.grpc.v0")

// ServerPoint registers a service.grpc provider on an SDK server and records
// the names that the daemon may publish on the API socket.
var ServerPoint = serverpoint.Registration{
	Point: Point.ID(),
	Register: func(r grpc.ServiceRegistrar, impl any) {
		impl.(Provider).RegisterServices(r)
	},
}
