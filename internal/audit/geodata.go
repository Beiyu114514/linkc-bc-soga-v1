package audit

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	xgeodata "github.com/xtls/xray-core/common/geodata"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"google.golang.org/protobuf/proto"
)

// GeoData is a lazily loaded subset of Xray geosite.dat / geoip.dat.
type GeoData struct {
	mu          sync.RWMutex
	sitesLoaded bool
	ipsLoaded   bool
	sites       map[string][]*xgeodata.Domain
	ips         map[string][]*net.IPNet
}

func newGeoData() *GeoData {
	return &GeoData{
		sites: make(map[string][]*xgeodata.Domain),
		ips:   make(map[string][]*net.IPNet),
	}
}

// LoadGeodata reads geosite.dat and geoip.dat from dir. Missing files are
// returned as errors, but any successfully loaded file remains usable.
func LoadGeodata(dir string) (*GeoData, error) {
	g := newGeoData()
	err := g.EnsureLoaded(dir, true, true)
	return g, err
}

// EnsureLoaded loads geosite.dat and/or geoip.dat when requested and not yet loaded.
func (g *GeoData) EnsureLoaded(dir string, needSites, needIPs bool) error {
	var errs []error
	g.mu.Lock()
	sitesNeeded := needSites && !g.sitesLoaded
	ipsNeeded := needIPs && !g.ipsLoaded
	g.mu.Unlock()

	if sitesNeeded {
		if b, err := os.ReadFile(filepath.Join(dir, "geosite.dat")); err != nil {
			errs = append(errs, fmt.Errorf("geosite.dat: %w", err))
		} else {
			var list xgeodata.GeoSiteList
			if err := proto.Unmarshal(b, &list); err != nil {
				errs = append(errs, fmt.Errorf("geosite.dat: %w", err))
			} else {
				sites := make(map[string][]*xgeodata.Domain, len(list.Entry))
				for _, e := range list.Entry {
					if e.GetCode() != "" {
						sites[strings.ToLower(e.GetCode())] = e.GetDomain()
					}
				}
				g.mu.Lock()
				g.sites = sites
				g.sitesLoaded = true
				g.mu.Unlock()
			}
		}
	}
	if ipsNeeded {
		if b, err := os.ReadFile(filepath.Join(dir, "geoip.dat")); err != nil {
			errs = append(errs, fmt.Errorf("geoip.dat: %w", err))
		} else {
			var list xgeodata.GeoIPList
			if err := proto.Unmarshal(b, &list); err != nil {
				errs = append(errs, fmt.Errorf("geoip.dat: %w", err))
			} else {
				ips := make(map[string][]*net.IPNet, len(list.Entry))
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
					ips[strings.ToLower(e.GetCode())] = nets
				}
				g.mu.Lock()
				g.ips = ips
				g.ipsLoaded = true
				g.mu.Unlock()
			}
		}
	}
	return errors.Join(errs...)
}

func needsGeodataKinds(rules []panel.AuditRule, white []string) (needSite, needIP bool) {
	check := func(raw string) {
		p, _, ok := splitPrefix(strings.TrimSpace(raw))
		if !ok {
			return
		}
		if p == "geosite" {
			needSite = true
		} else if p == "geoip" {
			needIP = true
		}
	}
	for _, r := range rules {
		check(r.Rule)
	}
	for _, w := range white {
		check(w)
	}
	return needSite, needIP
}

func (g *GeoData) compileGeo(id int64, prefix, value string) ([]compiled, error) {
	if g == nil {
		return nil, fmt.Errorf("%s rules require geodata.dir with geosite.dat/geoip.dat", prefix)
	}
	rawCode := strings.ToLower(strings.TrimSpace(value))
	code, attr, hasAttr := strings.Cut(rawCode, "@")
	code = strings.TrimSpace(code)
	attr = strings.TrimSpace(attr)
	switch prefix {
	case "geosite":
		g.mu.RLock()
		domains := g.sites[code]
		g.mu.RUnlock()
		if len(domains) == 0 {
			return nil, fmt.Errorf("geosite code %q not found", code)
		}
		out := make([]compiled, 0, len(domains))
		for _, d := range domains {
			if hasAttr && attr != "" && !domainHasAttr(d, attr) {
				continue
			}
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
		if len(out) == 0 {
			return nil, fmt.Errorf("geosite %q has no matching entries", rawCode)
		}
		return out, nil
	case "geoip":
		if hasAttr && attr != "" {
			return nil, fmt.Errorf("geoip does not support attribute filter %q", rawCode)
		}
		g.mu.RLock()
		nets := g.ips[code]
		g.mu.RUnlock()
		if len(nets) == 0 {
			return nil, fmt.Errorf("geoip code %q not found", code)
		}
		return []compiled{{id: id, kind: kindIP, cidrs: nets}}, nil
	}
	return nil, fmt.Errorf("unknown geodata prefix %q", prefix)
}

func domainHasAttr(d *xgeodata.Domain, attr string) bool {
	for _, a := range d.GetAttribute() {
		if strings.EqualFold(a.GetKey(), attr) {
			return true
		}
	}
	return false
}
