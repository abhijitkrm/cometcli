package triage

import "testing"

// The round state a node stuck at a height reports when the biggest
// validator is down: its bit is unset in every round.
func TestConsensusVotesNamesTheAbsent(t *testing.T) {
	raw := []byte(`{"height":"73005","validators":{"validators":[
		{"address":"0fb5d14dca750c9e64b91bb747a437d49191c647","voting_power":"10000100"},
		{"address":"AAAA","voting_power":"10000000"},
		{"address":"BBBB","voting_power":"1980350"}]},
	"votes":[
		{"round":0,"prevotes_bit_array":"BA{3:_xx} 11980350/21980450 = 0.55","precommits_bit_array":"BA{3:___} 0/21980450 = 0.00"},
		{"round":1,"prevotes_bit_array":"BA{3:___} 0/21980450 = 0.00","precommits_bit_array":"BA{3:___} 0/21980450 = 0.00"}]}`)
	v := ConsensusVotes(raw)
	if v == nil || v.VotedPct != 54.5 || len(v.Absent) != 1 || v.Absent[0].Addr != "0FB5D14DCA750C9E64B91BB747A437D49191C647" || v.Absent[0].Pct != 45.5 {
		t.Fatalf("got %+v", v)
	}
	if ConsensusVotes([]byte(`{"votes":[]}`)) != nil {
		t.Fatal("no validators: no verdict")
	}
}
