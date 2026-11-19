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

// Package reconcileext is the daemon-side half of the reconcile point: an
// extension of the moby daemon, shipped in the Compose binary, that converges
// the daemon's resources on a Compose model by using pkg/compose as a library.
//
// # How the daemon launches it
//
// The daemon (moby/extensions, internal/launcher) scans its extension directory
// (default /usr/libexec/docker/moby-extensions) and starts every binary found
// there, the name of the file being the extension ID:
//
//   - argv is the bare binary path: argv[0] is the full path of the file, no
//     further argument is passed;
//   - the environment is the daemon's own (nothing is added) and the working
//     directory is the daemon's;
//   - one JSON document (the startup configuration: socket path, protocol
//     version, the extension's entry of daemon.json "extensions.configs",
//     callback socket for dependencies) is written on stdin;
//   - the extension listens on that unix socket, then writes "ready\n" on
//     stdout, which is reserved for that acknowledgement;
//   - the daemon then calls Describe and, in dependency order, Initialize over
//     gRPC on the socket; stderr is folded into the daemon logs.
//
// Since the arguments and the environment carry nothing that identifies the
// extension, the Compose binary recognizes the mode by its own file name, as
// busybox does: see [Selected].
package reconcileext

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/moby/extensions/sdk"

	reconcilev0 "github.com/docker/compose/v5/internal/reconcileapi"
	daemonapipb "github.com/docker/compose/v5/internal/reconcileext/daemonapi/protogen"
	"github.com/docker/compose/v5/internal/reconcileext/servicegrpc"
)

// ModeFlag is the explicit way to select the extension mode, for running the
// binary by hand under another name (debugging). It must be the only argument:
// the daemon never passes one, the file name does the selection.
const ModeFlag = "--moby-extension"

// Selected reports whether the process must run as the daemon extension rather
// than as the Compose CLI. It only looks at argv, so it costs nothing in the
// CLI's normal path (including the Docker CLI plugin mode, which runs under the
// name docker-compose with its own arguments).
func Selected(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if len(args) == 2 && args[1] == ModeFlag {
		return true
	}
	return binaryName(args[0]) == reconcilev0.ExtensionID
}

// binaryName is the name the daemon identifies a binary by: the file name,
// without the .exe suffix.
func binaryName(argv0 string) string {
	// the daemon may be on another OS than the one running the tests: do not
	// depend on filepath's separator alone
	name := argv0
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, ".exe")
}

// Main runs the extension until the daemon stops it, then exits the process.
func Main() {
	if err := run(); err != nil {
		// stdout is reserved for the readiness acknowledgement; the daemon
		// captures stderr and folds it into its logs
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	// the daemon stops an extension with SIGTERM: cancelling the context lets
	// the SDK shut the extension down and exit cleanly
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := sdk.NewServer()
	if err := srv.Register(New(), servicegrpc.ServerPoint); err != nil {
		return err
	}
	srv.Depends(daemonapipb.ClientPoint)
	return srv.Listen(ctx)
}
