package tx

import "testing"

func TestJSONFixupsReachNestedMessages(t *testing.T) {
	JSONFixups["/x.v1.MsgUpdateParams"] = func(o map[string]any) { o["params"] = "fixed" }
	defer delete(JSONFixups, "/x.v1.MsgUpdateParams")
	tx := map[string]any{"@type": "/cosmos.gov.v1.MsgSubmitProposal", "messages": []any{
		map[string]any{"@type": "/x.v1.MsgUpdateParams", "params": "scaled"},
	}}
	fixJSON(tx)
	if got := tx["messages"].([]any)[0].(map[string]any)["params"]; got != "fixed" {
		t.Fatalf("nested message not fixed: %v", got)
	}
}
