package audit

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	xgeodata "github.com/xtls/xray-core/common/geodata"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"google.golang.org/protobuf/proto"
)

// GeoData is a lazily loaded subset of Xray geosite.dat / geoip.dat.
type GeoData struct {
	sites map[string][]*xgeodata.Domain
	ips   map[string][]*net.IPNet
}

// LoadGeodata reads geosite.dat and geoip.dat from dir. Missing files are
// returned as errors, but any successfully loaded file remains usable.
func LoadGeodata(dir string) (*GeoData, error) {
	g := &GeoData{sites: make(map[string][]*xgeodata.Domain), ips: make(map[string][]*net.IPNet)}
	var errs []error
	if b, err := os.ReadFile(filepath.Join(dir, "geosite.dat")); err != nil {
		errs = append(errs, fmt.Errorf("geosite.dat: %w", err))
	} else {
		var list xgeodata.GeoSiteList
		if err := proto.Unmarshal(b, &list); err != nil {
			errs = append(errs, fmt.Errorf("geosite.dat: %w", err))
		} else {
			for _, e := range list.Entry {
				if e.GetCode() != "" {
					g.sites[strings.ToLower(e.GetCode())] = e.GetDomain()
				}
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "geoip.dat")); err != nil {
		errs = append(errs, fmt.Errorf("geoip.dat: %w", err))
	} else {
		var list xgeodata.GeoIPList
		if err := proto.Unmarshal(b, &list); err != nil {
			errs = append(errs, fmt.Errorf("geoip.dat: %w", err))
		} else {
			for _, e := range list.Entry {
				if e.GetCode() == "" {
					continue
				}
				var nets []*net.IPNet
				for _, c := range e.GetCidr() {
					bits := len(c.GetIp()) * 8
					if bits != 32 && bits != 128 {
						continue
					}
					mask := net.CIDRMask(int(c.GetPrefix()), bits)
					if mask == nil {
						continue
					}
					ip := net.IP(c.GetIp())
					nets = append(nets, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
				}
				g.ips[strings.ToLower(e.GetCode())] = nets
			}
		}
	}
	return g, errors.Join(errs...)
}

func needsGeodata(rules []panel.AuditRule, white []string) bool {
	for _, r := range rules {
		if hasGeoPrefix(r.Rule) {
			return true
		}
	}
	for _, w := range white {
		if hasGeoPrefix(w) {
			return true
		}
	}
	return false
}

func hasGeoPrefix(raw string) bool {
	p, _, ok := splitPrefix(strings.TrimSpace(raw))
	return ok && (p == "geosite" || p == "geoip")
}

func (g *GeoData) compileGeo(id int64, prefix, value string) ([]compiled, error) {
	if g == nil {
		return nil, fmt.Errorf("%s rules require geodata.dir with geosite.dat/geoip.dat", prefix)
	}
	code := strings.ToLower(strings.TrimSpace(value))
	if i := strings.IndexByte(code, '@'); i >= 0 {
		code = code[:i]
	}
	switch prefix {
	case "geosite":
		domains := g.sites[code]
		if len(domains) == 0 {
			return nil, fmt.Errorf("geosite code %q not found", code)
		}
		out := make([]compiled, 0, len(domains))
		for _, d := range domains {
			switch d.GetType() {
			case xgeodata.Domain_Substr:
				out = append(out, compiled{id: id, kind: kindKeyword, value: strings.ToLower(d.GetValue())})
			case xgeodata.Domain_Domain:
				out = append(out, compiled{id: id, kind: kindDomain, value: strings.ToLower(d.GetValue())})
			case xgeodata.Domain_Full:
				out = append(out, compiled{id: id, kind: kindFull, value: strings.ToLower(d.GetValue())})
			case xgeodata.Domain_Regex:
				re, err := regexpCompile(d.GetValue())
				if err != nil {
					continue
				}
				out = append(out, compiled{id: id, kind: kindRegex, re: re})
			}
		}
		return out, nil
	case "geoip":
		nets := g.ips[code]
		if len(nets) == 0 {
			return nil, fmt.Errorf("geoip code %q not found", code)
		}
		return []compiled{{id: id, kind: kindIP, cidrs: nets}}, nil
	}
	return nil, fmt.Errorf("unknown geodata prefix %q", prefix)
}
