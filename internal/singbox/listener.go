package singbox

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/jsjson"
	"github.com/Ametas/route-agent/internal/runner"
)

// CoreNotListeningKind is the warning class the orchestrator deduplicates on.
const CoreNotListeningKind = "core_not_listening"

// PublicListenPorts is publicListenPorts (listenerWarning.ts:36-55): listen_port of every inbound
// whose listen is empty, "0.0.0.0" or "::" (127.0.0.1 and any specific IP are not public), when
// it is an integer > 0. Deduplicated, in order. type and network are ignored (spec B R2, kept).
func PublicListenPorts(cfg any) []float64 {
	inbounds, _ := inboundsOf(cfg)
	var out []float64
	seen := map[float64]bool{}
	for _, raw := range inbounds {
		in, ok := raw.(*jsjson.Object)
		if !ok {
			continue
		}
		listen := ""
		if l, ok := in.Get("listen"); ok {
			listen, _ = l.(string)
		}
		if listen != "" && listen != "0.0.0.0" && listen != "::" {
			continue
		}
		pv, _ := in.Get("listen_port")
		port, ok := pv.(float64)
		if ok && jsjson.IsInteger(port) && port > 0 && !seen[port] {
			seen[port] = true
			out = append(out, port)
		}
	}
	return out
}

var ssLineRe = regexp.MustCompile(`[\s%][^\s]*:(\d+)\s+[^\s]+:\*`)

// ParseListeningPorts is parseListeningPorts: the LOCAL port of each `ss -ln?` line
// ("<local>:<port>  <peer>:*"), deduplicated.
func ParseListeningPorts(out string) []float64 {
	var ports []float64
	seen := map[float64]bool{}
	for _, line := range strings.Split(out, "\n") {
		m := ssLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil || !jsjson.IsInteger(n) || seen[n] {
			continue
		}
		seen[n] = true
		ports = append(ports, n)
	}
	return ports
}

func joinPorts(ps []float64) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = jsjson.FormatNumber(p)
	}
	return strings.Join(parts, ", ")
}

// DetectCoreNotListening is detectCoreNotListening (listenerWarning.ts:78-115), for
// PullNodeWarnings (group E, which puts it first in the list): nil when the applied config is
// missing, empty or unparsable, declares no public port (ss is then not run), or ss fails;
// otherwise a warning when some declared port is in neither `ss -lnu` nor `ss -lnt`.
func (s *Service) DetectCoreNotListening(ctx context.Context) *agentpb.NodeWarning {
	raw, err := s.FS.ReadFile(s.configPath())
	if err != nil || len(raw) == 0 {
		return nil
	}
	cfg, err := jsjson.Parse(string(raw))
	if err != nil {
		return nil
	}
	expected := PublicListenPorts(cfg)
	if len(expected) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	var udp, tcp runner.Result
	var udpErr, tcpErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		udp, udpErr = s.Runner.Run(ctx, runner.Cmd{Name: "ss", Args: []string{"-lnu"}, Timeout: ssTimeout})
	}()
	go func() {
		defer wg.Done()
		tcp, tcpErr = s.Runner.Run(ctx, runner.Cmd{Name: "ss", Args: []string{"-lnt"}, Timeout: ssTimeout})
	}()
	wg.Wait()
	if udpErr != nil || tcpErr != nil {
		e := udpErr
		if e == nil {
			e = tcpErr
		}
		s.Log.Debug("Listener check skipped: ss is unavailable", "err", e.Error())
		return nil
	}
	listening := map[float64]bool{}
	for _, p := range append(ParseListeningPorts(udp.Stdout), ParseListeningPorts(tcp.Stdout)...) {
		listening[p] = true
	}
	var missing []float64
	for _, p := range expected {
		if !listening[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &agentpb.NodeWarning{
		Kind:           CoreNotListeningKind,
		Source:         "sing-box",
		Sample:         "конфиг объявляет порты " + joinPorts(expected) + ", но никто не слушает " + joinPorts(missing),
		Occurrences:    uint32(len(missing)),
		LastSeenUnixMs: s.now().UnixMilli(),
	}
}
