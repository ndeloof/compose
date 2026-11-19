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
	"testing"

	"gotest.tools/v3/assert"
)

func TestSelected(t *testing.T) {
	const dir = "/usr/libexec/docker/moby-extensions/"
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"daemon launch: full path, no argument", []string{dir + "io.docker.compose.reconcile.v0"}, true},
		{"bare name", []string{"io.docker.compose.reconcile.v0"}, true},
		{"windows name", []string{`C:\ext\io.docker.compose.reconcile.v0.exe`}, true},
		{"explicit flag under any name", []string{"/usr/bin/docker-compose", ModeFlag}, true},
		{"standalone CLI", []string{"/usr/local/bin/docker-compose", "up", "-d"}, false},
		{"plugin metadata", []string{"/usr/lib/docker/cli-plugins/docker-compose", "docker-cli-plugin-metadata"}, false},
		{"plugin run", []string{"/usr/lib/docker/cli-plugins/docker-compose", "compose", "up"}, false},
		{"flag among other arguments is the CLI's", []string{"/usr/bin/docker-compose", "up", ModeFlag}, false},
		{"another extension's name", []string{dir + "org.mobyproject.jobs.v0"}, false},
		{"name as a prefix only", []string{dir + "io.docker.compose.reconcile.v01"}, false},
		{"name as a directory", []string{"/io.docker.compose.reconcile.v0/docker-compose"}, false},
		{"no argv", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, Selected(tt.args), tt.want)
		})
	}
}
