package govtool

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// a feemarket-like module as a chain would describe it over reflection
func feemarketFiles(t *testing.T) *protoregistry.Files {
	t.Helper()
	decOpt := &descriptorpb.FieldOptions{}
	var unk []byte
	unk = protowire.AppendTag(unk, 93002, protowire.BytesType)
	unk = protowire.AppendString(unk, "cosmos.Dec")
	decOpt.ProtoReflect().SetUnknown(unk)
	str, i64, msg := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(), descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	field := func(name string, n int32, typ *descriptorpb.FieldDescriptorProto_Type, typeName string, o *descriptorpb.FieldOptions) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(n), Type: typ, Label: opt, JsonName: proto.String(name), Options: o}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	fd := &descriptorpb.FileDescriptorProto{
		Name: proto.String("cosmos/evm/feemarket/v1/x.proto"), Package: proto.String("cosmos.evm.feemarket.v1"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Params"), Field: []*descriptorpb.FieldDescriptorProto{
				field("base_fee_change_denominator", 1, i64, "", nil),
				field("min_gas_multiplier", 2, str, "", decOpt),
				field("note", 3, str, "", nil),
			}},
			{Name: proto.String("MsgUpdateParams"), Field: []*descriptorpb.FieldDescriptorProto{
				field("authority", 1, str, "", nil), field("params", 2, msg, ".cosmos.evm.feemarket.v1.Params", nil),
			}},
			{Name: proto.String("QueryParamsRequest")},
			{Name: proto.String("QueryParamsResponse"), Field: []*descriptorpb.FieldDescriptorProto{field("params", 1, msg, ".cosmos.evm.feemarket.v1.Params", nil)}},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("Query"), Method: []*descriptorpb.MethodDescriptorProto{
			{Name: proto.String("Params"), InputType: proto.String(".cosmos.evm.feemarket.v1.QueryParamsRequest"), OutputType: proto.String(".cosmos.evm.feemarket.v1.QueryParamsResponse")},
		}}},
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fd}})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestFindModuleAndDecFields(t *testing.T) {
	files := feemarketFiles(t)
	api, err := FindModule(files, "feemarket")
	if err != nil {
		t.Fatal(err)
	}
	if api.Method != "/cosmos.evm.feemarket.v1.Query/Params" || api.Msg.FullName() != "cosmos.evm.feemarket.v1.MsgUpdateParams" {
		t.Errorf("api = %s %s", api.Method, api.Msg.FullName())
	}
	if len(api.decPaths) != 1 || strings.Join(api.decPaths[0], ".") != "min_gas_multiplier" {
		t.Errorf("dec fields = %v (note is a plain string)", api.decPaths)
	}
	if _, err := FindModule(files, "staking"); err == nil || !strings.Contains(err.Error(), "feemarket") {
		t.Errorf("unknown module should list the ones there: %v", err)
	}
}

func TestApplyChanges(t *testing.T) {
	params := map[string]any{
		"active_static_precompiles": []any{"0x0000000000000000000000000000000000000800", "0x0000000000000000000000000000000000000804"},
		"history_serve_window":      "8192", // uint64: a string in proto JSON
		"access_control":            map[string]any{"create": map[string]any{"access_type": "ACCESS_TYPE_PERMISSIONLESS"}},
		"min_gas_multiplier":        "0.5",
	}
	ch, err := ParseChanges("active_static_precompiles+=ics02, bank; history_serve_window=16384\naccess_control.create.access_type=ACCESS_TYPE_RESTRICTED; min_gas_multiplier=0.4")
	if err != nil {
		t.Fatal(err)
	}
	diff, err := ApplyChanges(params, ch)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(diff, "\n")
	for _, want := range []string{"+ 0x0000000000000000000000000000000000000807 (ics02) (2 → 3 items)", `history_serve_window: "8192" → "16384"`,
		`access_control.create.access_type: "ACCESS_TYPE_PERMISSIONLESS" → "ACCESS_TYPE_RESTRICTED"`, `min_gas_multiplier: "0.5" → "0.4"`} {
		if !strings.Contains(got, want) {
			t.Errorf("diff missing %q:\n%s", want, got)
		}
	}
	// bank was already there; the list is sorted (evmd requires it)
	if l := params["active_static_precompiles"].([]any); len(l) != 3 || l[2] != "0x0000000000000000000000000000000000000807" {
		t.Errorf("precompiles = %v", l)
	}
	if _, err := ApplyChanges(params, []Change{{Path: []string{"no_such"}, Op: "=", Raw: "1"}}); err == nil || !strings.Contains(err.Error(), "history_serve_window") {
		t.Errorf("unknown field should list the real ones: %v", err)
	}
	if _, err := ApplyChanges(params, []Change{{Path: []string{"min_gas_multiplier"}, Op: "=", Raw: "0.4"}}); err == nil || !strings.Contains(err.Error(), "nothing to propose") {
		t.Errorf("a no-op change must be refused: %v", err)
	}
	if _, err := ApplyChanges(params, []Change{{Path: []string{"min_gas_multiplier"}, Op: "+=", Raw: "1"}}); err == nil {
		t.Error("+= on a scalar must be refused")
	}
	if _, err := ParseChanges("just words"); err == nil {
		t.Error("a change without an operator must be refused")
	}
}

func TestPlainDec(t *testing.T) {
	for in, want := range map[string]string{"500000000000000000": "0.5", "1000000000000000000000000000": "1000000000", "0": "0", "0.25": "0.25"} {
		if got := plainDec(in); got != want {
			t.Errorf("plainDec(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestScaleDecIsExact(t *testing.T) {
	// a whole-number decimal is a value, not an already-scaled one
	for in, want := range map[string]string{"1000000000": "1000000000000000000000000000", "0.4": "400000000000000000", "0": "0"} {
		if got, err := scaleDec(in); err != nil || got != want {
			t.Errorf("scaleDec(%s) = %s %v, want %s", in, got, err, want)
		}
	}
	if _, err := scaleDec("0.0000000000000000001"); err == nil {
		t.Error("more than 18 places must be refused")
	}
}
