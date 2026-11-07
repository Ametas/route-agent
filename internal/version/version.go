// Package version reports the agent version for GetAgentInfo (spec A §7.2, §7.3).
//
// The orchestrator parses it with ^v?(\d+\.\d+\.\d+)(?:-\d+-g[0-9a-f]+)?$ and self-updates any
// node whose base version is unparsable or behind the fleet target, every 6 h. So the shape must
// stay what `git describe --tags --always` prints: vX.Y.Z on a tag, vX.Y.Z-N-gHASH after it.
//
// Source order (the TS chain was: git describe in cwd → package.json → "1.0.0"):
//  1. Version, injected at build time:
//     go build -ldflags "-X github.com/Ametas/route-agent/internal/version.Version=$(git describe --tags --always)"
//  2. `git describe --tags --always` in the working directory (dev builds, go run);
//  3. "version" from ./package.json;
//  4. "1.0.0".
package version

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Ametas/route-agent/internal/runner"
)

// Version is set with -ldflags -X at build time. Empty in plain `go build`/`go test`.
var Version = ""

// shapeRe is the orchestrator's parser (orch:src/core/services/agentCatchUp.ts:45-60).
var shapeRe = regexp.MustCompile(`^v?(\d+\.\d+\.\d+)(?:-\d+-g[0-9a-f]+)?$`)

// Valid reports whether v has the release shape the orchestrator understands.
func Valid(v string) bool { return shapeRe.MatchString(v) }

// Resolver computes the version once per process.
type Resolver struct {
	Runner runner.Runner
	// Dir is where git and package.json are looked up; "" is the cwd.
	Dir string

	once sync.Once
	v    string
}

// Get returns the version, resolving it on first use.
func (r *Resolver) Get(ctx context.Context) string {
	r.once.Do(func() { r.v = r.resolve(ctx) })
	return r.v
}

func (r *Resolver) resolve(ctx context.Context) string {
	if v := strings.TrimSpace(Version); v != "" {
		return v
	}
	if r.Runner != nil {
		res, err := r.Runner.Run(ctx, runner.Cmd{
			Name: "git", Args: []string{"describe", "--tags", "--always"},
			Timeout: 5 * time.Second, Dir: r.Dir,
		})
		if err == nil {
			if v := strings.TrimSpace(res.Stdout); v != "" {
				return v
			}
		}
	}
	pkgPath := "package.json"
	if r.Dir != "" {
		pkgPath = r.Dir + "/package.json"
	}
	if data, err := os.ReadFile(pkgPath); err == nil {
		var pkg struct {
			Version any `json:"version"`
		}
		if json.Unmarshal(data, &pkg) == nil {
			if s, ok := pkg.Version.(string); ok && s != "" {
				return s
			}
		}
	}
	return "1.0.0"
}
