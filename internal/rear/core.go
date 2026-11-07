package rear

import (
	"context"
	"os"
	"path"
	"strings"

	"github.com/Ametas/route-agent/internal/clashapi"
	"github.com/Ametas/route-agent/internal/jsjson"
)

// CoreID names a rear core.
type CoreID string

const (
	// Singbox: the rear runs on the front's own sing-box binary.
	Singbox CoreID = "singbox"
	// Mihomo: the rear runs on mihomo (rule-providers, spec F §1.2).
	Mihomo CoreID = "mihomo"
)

// Defaults behind the TS `config.X || '<default>'` fallbacks of rearCore.ts:88-90, 117-119.
const (
	DefaultSingboxBinary     = "/usr/local/bin/sing-box"
	DefaultSingboxConfigPath = "/etc/route-agent/rear.json"
	DefaultSingboxUnitPath   = "/etc/systemd/system/route-rear-singbox.service"
	DefaultMihomoBinary      = "/usr/local/bin/mihomo"
	DefaultMihomoConfigPath  = "/etc/route-agent/rear.yaml"
	DefaultMihomoUnitPath    = "/etc/systemd/system/route-rear-mihomo.service"
	// LegacyConfigPath is where the sing-box rear config lived until 2026-09-04; left there it
	// would be merged into the front config by any `sing-box -C /etc/sing-box`.
	LegacyConfigPath = "/etc/sing-box/rear.json"
)

// WarpMembers are the members of the `warp` selector the guard switches between. They differ per
// core and mihomo rejects a PUT with the other core's name (rearCore.ts:29-36).
type WarpMembers struct {
	// Pool is selected while WARP has at least one live key.
	Pool string
	// Direct is selected when no key is alive.
	Direct string
}

// Core describes one rear core: the differences between the two are data, not branches
// (rearCore.ts:17-20).
type Core struct {
	ID          CoreID
	Label       string // human-readable name in messages ("sing-box", "mihomo")
	BinaryPath  string
	ConfigPath  string
	UnitPath    string
	WarpMembers WarpMembers
}

// UnitName is basename(unitPath, '.service').
func (c Core) UnitName() string { return strings.TrimSuffix(path.Base(c.UnitPath), ".service") }

// WorkDir is the directory of the config (mihomo's -d home: rule sets must live under it).
func (c Core) WorkDir() string { return path.Dir(c.ConfigPath) }

// UnitContent is the byte-exact unit text (rearCore.ts:67-147). Any byte difference would rewrite
// the unit and RESTART the rear on the first push after the switchover.
func (c Core) UnitContent() string {
	var desc, start, reload string
	if c.ID == Mihomo {
		wd := c.WorkDir()
		desc = "Rear mihomo (WARP egress with rule sets, managed by route-agent)"
		start = c.BinaryPath + " -d " + wd + " -f " + c.ConfigPath
		// SIGHUP, not restart: mihomo re-reads its config on the signal without exiting (verified
		// on a live binary 2026-09-06), and the whole star traffic goes through the rear.
		reload = `/bin/sh -c "` + c.BinaryPath + " -t -d " + wd + " -f " + c.ConfigPath + ` && /bin/kill -HUP $MAINPID"`
	} else {
		desc = "Rear sing-box (WARP egress, managed by route-agent)"
		start = c.BinaryPath + " run -c " + c.ConfigPath
		reload = `/bin/sh -c "` + c.BinaryPath + " check -c " + c.ConfigPath + ` && /bin/kill -HUP $MAINPID"`
	}
	return "[Unit]\n" +
		"Description=" + desc + "\n" +
		"After=network-online.target\n" +
		"Wants=network-online.target\n" +
		"\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"ExecStart=" + start + "\n" +
		"Restart=on-failure\n" +
		"RestartSec=5\n" +
		"ExecReload=" + reload + "\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=multi-user.target\n"
}

// clashAPIFrom reads the Clash API address and secret from a PARSED applied config, in the
// core's own shape: sing-box `experimental.clash_api.external_controller`/`secret`, mihomo the
// top-level `external-controller`/`secret`. A blank address is "no API".
func (c Core) clashAPIFrom(parsed any) *clashapi.Endpoint {
	var api any
	var addrKey string
	if c.ID == Mihomo {
		api, addrKey = parsed, "external-controller"
	} else {
		api, addrKey = get(get(parsed, "experimental"), "clash_api"), "external_controller"
	}
	addr, ok := get(api, addrKey).(string)
	if !ok || jsjson.Trim(addr) == "" {
		return nil
	}
	secret, _ := get(api, "secret").(string)
	return &clashapi.Endpoint{Address: jsjson.Trim(addr), Secret: secret}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// SingboxCore is SINGBOX_REAR_CORE for this agent's settings.
func (s *Service) SingboxCore() Core {
	return Core{
		ID: Singbox, Label: "sing-box",
		BinaryPath:  orDefault(s.Cfg.SingboxBinaryPath, DefaultSingboxBinary),
		ConfigPath:  orDefault(s.Cfg.RearSingboxConfigPath, DefaultSingboxConfigPath),
		UnitPath:    orDefault(s.Cfg.RearSingboxUnitFilePath, DefaultSingboxUnitPath),
		WarpMembers: WarpMembers{Pool: "wg-pool", Direct: "direct"},
	}
}

// MihomoCore is MIHOMO_REAR_CORE for this agent's settings.
func (s *Service) MihomoCore() Core {
	return Core{
		ID: Mihomo, Label: "mihomo",
		BinaryPath:  orDefault(s.Cfg.MihomoBinaryPath, DefaultMihomoBinary),
		ConfigPath:  orDefault(s.Cfg.RearMihomoConfigPath, DefaultMihomoConfigPath),
		UnitPath:    orDefault(s.Cfg.RearMihomoUnitFilePath, DefaultMihomoUnitPath),
		WarpMembers: WarpMembers{Pool: "warp-auto", Direct: "DIRECT"},
	}
}

// ResolveCore is resolveRearCore: exactly "mihomo" is mihomo; anything else, including the proto3
// default "" of older orchestrators, is sing-box.
func (s *Service) ResolveCore(id string) Core {
	if id == string(Mihomo) {
		return s.MihomoCore()
	}
	return s.SingboxCore()
}

// otherCore is the core a switch removes.
func (s *Service) otherCore(c Core) Core {
	if c.ID == Mihomo {
		return s.SingboxCore()
	}
	return s.MihomoCore()
}

func (s *Service) exists(p string) bool {
	_, err := os.Stat(s.FS.Path(p))
	return err == nil
}

// ActiveCore is resolveActiveRearCore: the core whose config is on disk, mihomo first (its unit is
// the one brought up last); nil when neither exists.
func (s *Service) ActiveCore() *Core {
	for _, c := range []Core{s.MihomoCore(), s.SingboxCore()} {
		if s.exists(c.ConfigPath) {
			return &c
		}
	}
	return nil
}

// ActiveAPI is the Clash API of the running rear together with the core it belongs to.
type ActiveAPI struct {
	Core     Core
	Endpoint clashapi.Endpoint
}

// ActiveClashAPI is readActiveRearClashApi: the active core's applied config parsed as JSON (the
// mihomo config is written as JSON into its .yaml, so a hand-written real YAML config reads as
// "no rear", as in TS) and its Clash API. nil when there is no rear, the config is unreadable or
// unparsable, or it carries no address.
func (s *Service) ActiveClashAPI() *ActiveAPI {
	c := s.ActiveCore()
	if c == nil {
		return nil
	}
	raw, err := s.FS.ReadFile(c.ConfigPath)
	if err != nil {
		return nil
	}
	parsed, err := jsjson.Parse(string(raw))
	if err != nil {
		return nil
	}
	ep := c.clashAPIFrom(parsed)
	if ep == nil {
		return nil
	}
	return &ActiveAPI{Core: *c, Endpoint: *ep}
}

// running is isRearRunning: `systemctl is-active <unit>` prints "active".
func (s *Service) running(ctx context.Context, c Core) bool {
	return s.Systemctl.Active(ctx, c.UnitName())
}

// get is optional chaining `v?.[k]`: a missing key or a non-object yields nil (undefined).
func get(v any, k string) any {
	o, ok := v.(*jsjson.Object)
	if !ok {
		return nil
	}
	x, _ := o.Get(k)
	return x
}
