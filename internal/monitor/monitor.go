// Package monitor collects a point-in-time validator Snapshot shared by the
// watch TUI, the alert engine, and the agent's context snapshot.
package monitor

import (
	"fmt"
	"time"

	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"

	"github.com/abhijitkrm/cometcli/internal/tools/common"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Snapshot is one point-in-time view of validator health.
type Snapshot struct {
	TS          time.Time `json:"ts"`
	Reachable   bool      `json:"reachable"`
	Height      int64     `json:"height"`
	CatchingUp  bool      `json:"catching_up"`
	Peers       int       `json:"peers"`
	VotingPower int64     `json:"voting_power"`
	Version     string    `json:"version"`

	// signing (requires grpc + signer)
	ConsAddr   string `json:"cons_address,omitempty"`
	Missed     int64  `json:"missed"`
	Window     int64  `json:"window"`
	UptimePct  float64 `json:"uptime_pct"`
	Jailed     bool   `json:"jailed"`
	Tombstoned bool   `json:"tombstoned"`

	// host
	DiskUsedPct float64 `json:"disk_used_pct"`
	MemUsedPct  float64 `json:"mem_used_pct"`
	ServiceUp   *bool   `json:"service_up,omitempty"`

	// evm (rpc/sentry profiles only)
	EVMHeight uint64 `json:"evm_height,omitempty"`
	EVMDrift  int64  `json:"evm_drift,omitempty"`

	Errors []string `json:"errors,omitempty"`
}

// Collect gathers a Snapshot using the context's lazy clients. Individual
// failures are recorded in Errors rather than aborting the whole snapshot.
func Collect(c *toolkit.Context) *Snapshot {
	s := &Snapshot{TS: time.Now()}

	if cc, err := c.Comet(); err == nil {
		if st, err := cc.Status(c); err == nil {
			s.Reachable = true
			s.Height = st.SyncInfo.LatestBlockHeight
			s.CatchingUp = st.SyncInfo.CatchingUp
			s.VotingPower = st.ValidatorInfo.VotingPower
			s.Version = st.NodeInfo.Version
		} else {
			s.Errors = append(s.Errors, "status: "+err.Error())
		}
		if ni, err := cc.NetInfo(c); err == nil {
			s.Peers = int(ni.NPeers)
		}
	} else {
		s.Errors = append(s.Errors, "comet: "+err.Error())
	}

	// signing info — best effort
	if g, err := c.GRPC(); err == nil {
		if valoper, err := common.Valoper(c, nil); err == nil {
			if cons, err := common.ConsAddress(c, valoper); err == nil {
				s.ConsAddr = cons
				if si, err := g.Slashing.SigningInfo(c,
					&slashingv1beta1.QuerySigningInfoRequest{ConsAddress: cons}); err == nil {
					s.Missed = si.ValSigningInfo.MissedBlocksCounter
					s.Tombstoned = si.ValSigningInfo.Tombstoned
				}
				if p, err := g.Slashing.Params(c, &slashingv1beta1.QueryParamsRequest{}); err == nil {
					s.Window = p.Params.SignedBlocksWindow
					if s.Window > 0 {
						s.UptimePct = 100.0 * float64(s.Window-s.Missed) / float64(s.Window)
					}
				}
				if vres, err := g.Staking.Validator(c, validatorReq(valoper)); err == nil {
					s.Jailed = vres.Validator.Jailed
				}
			}
		}
	}

	// host stats
	if h, err := c.Host(); err == nil {
		if out, code, err := h.Run(c, "df -P "+c.Profile.Home+" 2>/dev/null | tail -1 | awk '{print $5}'"); err == nil && code == 0 {
			var pct float64
			if _, err := fmt.Sscanf(out, "%f%%", &pct); err == nil {
				s.DiskUsedPct = pct
			}
		}
		if c.Profile.Service.Unit != "" {
			out, _, _ := h.Run(c, "systemctl is-active "+c.Profile.Service.Unit+" 2>/dev/null || echo unknown")
			up := trim(out) == "active"
			s.ServiceUp = &up
		}
	}

	// evm plane if configured
	if c.Profile.Endpoints.EVM != "" {
		if ev, err := c.EVM(); err == nil {
			if eh, err := ev.BlockNumber(c); err == nil {
				s.EVMHeight = eh
				s.EVMDrift = s.Height - int64(eh)
			}
		}
	}
	return s
}

func validatorReq(valoper string) *stakingv1beta1.QueryValidatorRequest {
	return &stakingv1beta1.QueryValidatorRequest{ValidatorAddr: valoper}
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
