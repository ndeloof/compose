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
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/extensions/sdk"
	"github.com/moby/extensions/sdk/sdkapi"
	sdkapipb "github.com/moby/extensions/sdk/sdkapi/protogen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gotest.tools/v3/assert"

	reconcilev0 "github.com/docker/compose/v5/internal/reconcileapi"
	"github.com/docker/compose/v5/internal/reconcileext/daemonapi"
	daemonapipb "github.com/docker/compose/v5/internal/reconcileext/daemonapi/protogen"
	"github.com/docker/compose/v5/internal/reconcileext/servicegrpc"
)

// TestDaemonHandshake plays the daemon's side of the launch: the startup
// configuration on stdin, "ready" on stdout, then Describe over the socket.
// What the daemon reads in the declaration is what makes the point reachable on
// docker.sock (offered point, gRPC service) and the daemon API available to the
// extension (dependency).
func TestDaemonHandshake(t *testing.T) {
	// a unix socket path is short: the test's own directory may be too long
	dir, err := os.MkdirTemp("/tmp", "rext")
	assert.NilError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	endpoint := filepath.Join(dir, "ext.sock")

	srv := sdk.NewServer()
	assert.NilError(t, srv.Register(New(), servicegrpc.ServerPoint))
	srv.Depends(daemonapipb.ClientPoint)

	stdinR, stdinW, err := os.Pipe()
	assert.NilError(t, err)
	stdoutR, stdoutW, err := os.Pipe()
	assert.NilError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.ListenWithIO(ctx, stdinR, stdoutW) }()

	assert.NilError(t, json.NewEncoder(stdinW).Encode(sdk.StartupConfig{Endpoint: endpoint, ProtocolVersion: sdk.ProtocolVersion}))
	ready, err := bufio.NewReader(stdoutR).ReadString('\n')
	assert.NilError(t, err)
	assert.Equal(t, ready, sdk.ReadinessAck)

	conn, err := grpc.NewClient("unix:"+endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
		}))
	assert.NilError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	callCtx, callCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer callCancel()
	resp, err := sdkapipb.NewClient(conn).Describe(callCtx, &sdkapi.DescribeRequest{})
	assert.NilError(t, err)

	decl := resp.Declaration
	assert.Equal(t, decl.ID, reconcilev0.ExtensionID)
	// the daemon only routes what is published through service.grpc
	assert.DeepEqual(t, decl.OfferedPoints, []string{string(servicegrpc.Point.ID())})
	assert.Equal(t, len(decl.Dependencies), 1)
	assert.Equal(t, decl.Dependencies[0].Point, string(daemonapi.Point.ID()))
	assert.Equal(t, len(decl.ProviderServices), 1)
	assert.DeepEqual(t, decl.ProviderServices[0].Services, []string{"io.docker.compose.extension.reconcile.api.v0.Reconciler"})

	cancel()
	assert.NilError(t, <-done)
}

// TestDaemonAPIContractIsWireCompatible pins what makes the copy of moby's
// daemon.api point interoperable with the daemon.
func TestDaemonAPIContractIsWireCompatible(t *testing.T) {
	assert.Equal(t, string(daemonapi.Point.ID()), "org.mobyproject.extension.daemon.api.v0")
	assert.Equal(t, daemonapipb.ClientPoint.Point, daemonapi.Point.ID())
}
