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

package daemonapi

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/moby/moby/client"
)

// placeholderHost only selects the plain unix transport; the client dials the
// endpoint the provider reports.
const placeholderHost = "unix:///resolved-by-daemonapi-v0.sock"

// NewClient returns an Engine API client for the endpoint api reports. It is a
// copy of daemonapiv0.NewClient of moby: the daemon creates the socket when the
// client first connects. The caller owns the returned client and must close it.
func NewClient(api DaemonAPI, opts ...client.Opt) (*client.Client, error) {
	d := &dialer{api: api, lock: make(chan struct{}, 1)}
	return client.New(append([]client.Opt{
		client.WithHost(placeholderHost),
		client.WithDialContext(d.dial),
	}, opts...)...)
}

type dialer struct {
	api DaemonAPI

	// lock is a one-slot semaphore rather than a mutex so that dials waiting
	// on a slow Get still honor their own context.
	lock chan struct{}
	path string
}

func (d *dialer) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	path, err := d.socketPath(ctx)
	if err != nil {
		return nil, err
	}
	var nd net.Dialer
	return nd.DialContext(ctx, "unix", path)
}

func (d *dialer) socketPath(ctx context.Context) (string, error) {
	select {
	case d.lock <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-d.lock }()
	if d.path != "" {
		return d.path, nil
	}
	resp, err := d.api.Get(ctx, &GetRequest{})
	if err != nil {
		return "", fmt.Errorf("get daemon API endpoint: %w", err)
	}
	path, ok := strings.CutPrefix(resp.Host, "unix://")
	if !ok {
		return "", fmt.Errorf("unsupported daemon API endpoint %q", resp.Host)
	}
	d.path = path
	return path, nil
}
