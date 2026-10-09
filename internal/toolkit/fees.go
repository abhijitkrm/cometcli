package toolkit

import (
	"path"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/client/host"
)

// ParseMinGasPrices reads app.toml's minimum-gas-prices
// ("1000000000atest") as amount and denom — the first one when several
// are accepted.
func ParseMinGasPrices(appToml string) (amount, denom string) {
	for _, line := range strings.Split(appToml, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "minimum-gas-prices") {
			continue
		}
		_, v, _ := strings.Cut(line, "=")
		v = strings.Trim(strings.TrimSpace(v), `",`)
		v, _, _ = strings.Cut(v, ",")
		i := 0
		for i < len(v) && (v[i] >= '0' && v[i] <= '9' || v[i] == '.') {
			i++
		}
		return v[:i], strings.TrimSpace(v[i:])
	}
	return "", ""
}

// FeeDefaults fills metadata.fee_denom and gas_price from the node's own
// app.toml when the profile doesn't set them: fees are paid in what the
// node's mempool accepts. Read from the host home, or inside the
// container for docker nodes. Remembered for the process, not saved.
func (c *Context) FeeDefaults() {
	p := c.Profile
	if p == nil || (p.Metadata["fee_denom"] != "" && p.Metadata["gas_price"] != "") {
		return
	}
	h, err := c.Host()
	if err != nil {
		return
	}
	var raw string
	if p.Home != "" {
		if b, err := h.ReadFile(c, path.Join(p.Home, "config", "app.toml")); err == nil {
			raw = string(b)
		}
	}
	if raw == "" && p.Service.Type == "docker" && p.Service.Unit != "" {
		homes := []string{p.Signer.ContainerHome}
		if bin := p.Binary; bin != "" {
			homes = append(homes, "/root/."+bin, "/home/"+bin+"/."+bin)
		}
		for _, home := range homes {
			if home == "" {
				continue
			}
			sub, cancel := WithDeadline(c, 15*time.Second)
			res, err := host.Exec(sub, h, "docker exec "+shq(p.Service.Unit)+" cat "+shq(path.Join(home, "config", "app.toml")), 1<<20)
			cancel()
			if err == nil && res.Code == 0 {
				raw = res.Output
				break
			}
		}
	}
	amount, denom := ParseMinGasPrices(raw)
	if denom == "" {
		return
	}
	if p.Metadata == nil {
		p.Metadata = map[string]string{}
	}
	if p.Metadata["fee_denom"] == "" {
		p.Metadata["fee_denom"] = denom
	}
	if p.Metadata["gas_price"] == "" && amount != "" {
		p.Metadata["gas_price"] = amount
	}
}
