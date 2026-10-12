package govtool

// Parameter-change proposals. A module's MsgUpdateParams replaces its
// whole params object, so a field left out is reset to its default; the
// proposal is therefore built from the chain's current params with only
// the named changes applied. The message types come from the chain itself
// (cosmos.reflection.v1), so any module of any version works — cosmos/evm's
// vm, feemarket and erc20 included — without compiling its types in.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"

	reflectionv1 "cosmossdk.io/api/cosmos/reflection/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"

	grpcclient "github.com/abhijitkrm/cometcli/internal/client/grpc"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

// moduleAPI is a module's params query and its MsgUpdateParams.
type moduleAPI struct {
	Package  string
	Method   string // "/<pkg>.Query/Params"
	In, Out  protoreflect.MessageDescriptor
	Msg      protoreflect.MessageDescriptor
	Params   protoreflect.MessageDescriptor
	decPaths [][]string // fields typed cosmos.Dec
}

// moduleAlias maps what operators say to the module's package name.
var moduleAlias = map[string]string{"evm": "vm"}

var versionSegRe = regexp.MustCompile(`^v\d+((alpha|beta)\d*)?$`)

// ChainFiles fetches the chain's protobuf definitions.
func ChainFiles(c *toolkit.Context, g *grpcclient.Conn) (*protoregistry.Files, error) {
	resp := &reflectionv1.FileDescriptorsResponse{}
	if err := g.Invoke(c, "/cosmos.reflection.v1.ReflectionService/FileDescriptors", &reflectionv1.FileDescriptorsRequest{}, resp); err != nil {
		return nil, fmt.Errorf("the chain doesn't serve its protobuf definitions (cosmos.reflection.v1): %w", err)
	}
	return protodesc.FileOptions{AllowUnresolvable: true}.NewFiles(&descriptorpb.FileDescriptorSet{File: resp.Files})
}

// modulePackages are the packages that have a params query and a
// MsgUpdateParams, by module name.
func modulePackages(files *protoregistry.Files) map[string]string {
	out := map[string]string{}
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		pkg := string(fd.Package())
		segs := strings.Split(pkg, ".")
		name := ""
		for i := len(segs) - 1; i >= 0; i-- {
			if !versionSegRe.MatchString(segs[i]) {
				name = segs[i]
				break
			}
		}
		if name == "" || out[name] == pkg {
			return true
		}
		if _, err := files.FindDescriptorByName(protoreflect.FullName(pkg + ".MsgUpdateParams")); err != nil {
			return true
		}
		if d, err := files.FindDescriptorByName(protoreflect.FullName(pkg + ".Query")); err == nil {
			if s, ok := d.(protoreflect.ServiceDescriptor); ok && s.Methods().ByName("Params") != nil {
				out[name] = pkg
			}
		}
		return true
	})
	return out
}

// FindModule resolves a module name ("evm", "feemarket", "staking") to its
// params API on this chain.
func FindModule(files *protoregistry.Files, module string) (*moduleAPI, error) {
	want := strings.ToLower(strings.TrimSpace(module))
	if a := moduleAlias[want]; a != "" {
		want = a
	}
	pkgs := modulePackages(files)
	pkg, ok := pkgs[want]
	if !ok {
		var names []string
		for n := range pkgs {
			if n == "vm" {
				n = "evm"
			}
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("no module %q with updatable params on this chain — these have them: %s", module, strings.Join(names, ", "))
	}
	d, _ := files.FindDescriptorByName(protoreflect.FullName(pkg + ".Query"))
	m := d.(protoreflect.ServiceDescriptor).Methods().ByName("Params")
	md, _ := files.FindDescriptorByName(protoreflect.FullName(pkg + ".MsgUpdateParams"))
	msg := md.(protoreflect.MessageDescriptor)
	pf := msg.Fields().ByName("params")
	if pf == nil || pf.Message() == nil || m.Output().Fields().ByName("params") == nil {
		return nil, fmt.Errorf("%s.MsgUpdateParams has no params field", pkg)
	}
	api := &moduleAPI{Package: pkg, Method: "/" + pkg + ".Query/Params", In: m.Input(), Out: m.Output(), Msg: msg, Params: pf.Message()}
	api.decPaths = decFields(pf.Message(), nil, 0)
	return api, nil
}

// decFields are the paths of fields annotated cosmos.Dec (or a LegacyDec
// customtype): their encoding in a transaction depends on the SDK version.
func decFields(md protoreflect.MessageDescriptor, prefix []string, depth int) [][]string {
	var out [][]string
	if depth > 4 {
		return nil
	}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		path := append(append([]string{}, prefix...), string(f.Name()))
		switch {
		case f.Kind() == protoreflect.StringKind && !f.IsList() && isDec(f):
			out = append(out, path)
		case f.Kind() == protoreflect.MessageKind && !f.IsList() && !f.IsMap():
			out = append(out, decFields(f.Message(), path, depth+1)...)
		}
	}
	return out
}

// isDec reads the field's cosmos_proto.scalar (93002) and
// gogoproto.customtype (65003) options from their wire form.
func isDec(f protoreflect.FieldDescriptor) bool {
	opts, ok := f.Options().(*descriptorpb.FieldOptions)
	if !ok || opts == nil {
		return false
	}
	b, err := proto.Marshal(opts)
	if err != nil {
		return false
	}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return false
		}
		b = b[n:]
		if typ == protowire.BytesType {
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return false
			}
			s := string(v)
			if (num == 93002 && s == "cosmos.Dec") || (num == 65003 && strings.HasSuffix(s, "Dec")) {
				return true
			}
			b = b[m:]
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return false
		}
		b = b[m:]
	}
	return false
}

// CurrentParams queries the module's params as JSON (proto field names,
// every field present, decimals as plain decimals).
func CurrentParams(c *toolkit.Context, g *grpcclient.Conn, api *moduleAPI) (map[string]any, error) {
	req, resp := dynamicpb.NewMessage(api.In), dynamicpb.NewMessage(api.Out)
	if err := g.Invoke(c, api.Method, req, resp); err != nil {
		return nil, fmt.Errorf("%s params: %w", api.Package, err)
	}
	pm := resp.Get(api.Out.Fields().ByName("params")).Message()
	js, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(pm.Interface())
	if err != nil {
		return nil, err
	}
	params, err := decodeJSON(js)
	if err != nil {
		return nil, err
	}
	for _, p := range api.decPaths {
		if v, ok := getPath(params, p).(string); ok && v != "" {
			setPath(params, p, plainDec(v))
		}
	}
	return params, nil
}

func decodeJSON(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	return m, dec.Decode(&m)
}

// plainDec renders a decimal the chain returned — the legacy scaled
// integer or already a decimal — as a plain decimal ("0.5").
func plainDec(s string) string {
	if strings.ContainsAny(s, ".eE") {
		return s
	}
	r, ok := common.DecRat(s)
	if !ok {
		return s
	}
	out := r.FloatString(18)
	out = strings.TrimRight(strings.TrimRight(out, "0"), ".")
	if out == "" || out == "-" {
		return "0"
	}
	return out
}

func getPath(m map[string]any, path []string) any {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func setPath(m map[string]any, path []string, v any) {
	cur := m
	for _, k := range path[:len(path)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return
		}
		cur = next
	}
	cur[path[len(path)-1]] = v
}

// Change is one requested edit: field=value, list+=items, list-=items.
type Change struct {
	Path []string
	Op   string // "=", "+=", "-="
	Raw  string
}

var changeRe = regexp.MustCompile(`^\s*([A-Za-z0-9_.]+)\s*(\+=|-=|=)\s*(.*?)\s*$`)

// ParseChanges reads "a.b=1; list+=x,y" (';' or newline between changes).
func ParseChanges(s string) ([]Change, error) {
	var out []Change
	for _, part := range regexp.MustCompile(`[;\n]`).Split(s, -1) {
		if strings.TrimSpace(part) == "" {
			continue
		}
		m := changeRe.FindStringSubmatch(part)
		if m == nil {
			return nil, fmt.Errorf("change %q: want field=value, list+=item or list-=item", strings.TrimSpace(part))
		}
		out = append(out, Change{Path: strings.Split(m[1], "."), Op: m[2], Raw: m[3]})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no changes — pass set, e.g. set=\"active_static_precompiles+=ics02\"")
	}
	return out, nil
}

// listItems splits a list value; precompile names become their addresses.
func listItems(field, raw string) []any {
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(raw), "["), "]"))
	var out []any
	for _, x := range regexp.MustCompile(`[,\s]+`).Split(raw, -1) {
		if strings.EqualFold(x, "and") {
			continue
		}
		x = strings.Trim(strings.TrimSpace(x), `"`)
		if x == "" {
			continue
		}
		if field == "active_static_precompiles" && !strings.HasPrefix(x, "0x") {
			if a, ok := netspec.PrecompileAddr(x); ok {
				x = a
			}
		}
		out = append(out, x)
	}
	return out
}

func compact(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ApplyChanges edits params in place and returns "field: old → new" lines.
func ApplyChanges(params map[string]any, changes []Change) ([]string, error) {
	var diff []string
	for _, ch := range changes {
		key := strings.Join(ch.Path, ".")
		parent, ok := getPath(params, ch.Path[:len(ch.Path)-1]).(map[string]any)
		if len(ch.Path) == 1 {
			parent, ok = params, true
		}
		field := ch.Path[len(ch.Path)-1]
		before, exists := parent[field]
		if !ok || !exists {
			var keys []string
			for k := range parent {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return nil, fmt.Errorf("no field %s — the fields there are: %s", key, strings.Join(keys, ", "))
		}
		var after any
		list, isList := before.([]any)
		switch {
		case ch.Op != "=" && !isList:
			return nil, fmt.Errorf("%s isn't a list: use %s=value", key, key)
		case isList && ch.Op == "+=":
			next := append([]any{}, list...)
			for _, x := range listItems(field, ch.Raw) {
				if !containsVal(next, x) {
					next = append(next, x)
				}
			}
			after = next
		case isList && ch.Op == "-=":
			var next []any
			drop := listItems(field, ch.Raw)
			for _, x := range list {
				if !containsVal(drop, x) {
					next = append(next, x)
				}
			}
			if next == nil {
				next = []any{}
			}
			after = next
		case isList:
			after = listItems(field, ch.Raw)
		default:
			after = scalar(before, ch.Raw)
		}
		if field == "active_static_precompiles" {
			// evmd rejects unsorted precompiles
			if l, ok := after.([]any); ok {
				sort.Slice(l, func(i, j int) bool { return fmt.Sprint(l[i]) < fmt.Sprint(l[j]) })
			}
		}
		if compact(before) == compact(after) {
			return nil, fmt.Errorf("%s is already %s — nothing to propose", key, compact(before))
		}
		parent[field] = after
		if l, ok := after.([]any); ok && isList {
			diff = append(diff, key+": "+listDiff(field, list, l))
		} else {
			diff = append(diff, fmt.Sprintf("%s: %s → %s", key, compact(before), compact(after)))
		}
	}
	return diff, nil
}

func containsVal(l []any, v any) bool {
	for _, x := range l {
		if strings.EqualFold(fmt.Sprint(x), fmt.Sprint(v)) {
			return true
		}
	}
	return false
}

// scalar parses a new value in the shape of the old one: strings stay
// strings (numbers in proto JSON are often strings), JSON otherwise.
func scalar(before any, raw string) any {
	raw = strings.TrimSpace(raw)
	if b, ok := before.(string); ok {
		raw = strings.Trim(raw, `"`)
		// enums are upper case; chat lowercases what it reads
		if b != "" && b == strings.ToUpper(b) && strings.ContainsAny(b, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			raw = strings.ToUpper(raw)
		}
		return raw
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err == nil {
		return v
	}
	return raw
}

// ParamsProposal is a built MsgUpdateParams.
type ParamsProposal struct {
	Any    *anypb.Any
	Module string
	Diff   []string
	Keys   []string
}

// BuildParams reads the module's current params, applies the changes and
// packs the module's MsgUpdateParams with the gov module as authority.
func BuildParams(c *toolkit.Context, module string, changes []Change, prefix string) (*ParamsProposal, error) {
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	files, err := ChainFiles(c, g)
	if err != nil {
		return nil, err
	}
	api, err := FindModule(files, module)
	if err != nil {
		return nil, err
	}
	params, err := CurrentParams(c, g, api)
	if err != nil {
		return nil, err
	}
	diff, err := ApplyChanges(params, changes)
	if err != nil {
		return nil, err
	}
	// decimals as legacy scaled integers (0.4 → 400000000000000000): the
	// proposal's messages are unpacked by gogoproto, which decodes
	// LegacyDec that way even on SDK v0.54 (seen live: "cannot unmarshal
	// \"0.4\" into a *big.Int"); the gas simulation catches any mismatch
	for _, p := range api.decPaths {
		if v, ok := getPath(params, p).(string); ok && v != "" {
			if _, ok := new(big.Rat).SetString(v); !ok {
				return nil, fmt.Errorf("%s = %q isn't a decimal", strings.Join(p, "."), v)
			}
			enc, err := scaleDec(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", strings.Join(p, "."), err)
			}
			setPath(params, p, enc)
		}
	}
	gov, err := ModuleAddress("gov", prefix)
	if err != nil {
		return nil, err
	}
	js, err := json.Marshal(map[string]any{"authority": gov, "params": params})
	if err != nil {
		return nil, err
	}
	msg := dynamicpb.NewMessage(api.Msg)
	if err := (protojson.UnmarshalOptions{Resolver: dynamicpb.NewTypes(files)}).Unmarshal(js, msg); err != nil {
		return nil, fmt.Errorf("the changed params don't fit %s: %w", api.Msg.FullName(), err)
	}
	// so the transaction can be shown and signed as JSON like any other
	if _, err := protoregistry.GlobalTypes.FindMessageByName(api.Msg.FullName()); err != nil {
		_ = protoregistry.GlobalTypes.RegisterMessage(dynamicpb.NewMessageType(api.Msg))
	}
	// in JSON (signing inside the node's container) decimals are plain
	decs := api.decPaths
	tx.JSONFixups["/"+string(api.Msg.FullName())] = func(obj map[string]any) {
		pm, ok := obj["params"].(map[string]any)
		if !ok {
			return
		}
		for _, p := range decs {
			if v, ok := getPath(pm, p).(string); ok && v != "" {
				setPath(pm, p, plainDec(v))
			}
		}
	}
	a, err := cosmosAny(msg)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, ch := range changes {
		keys = append(keys, strings.Join(ch.Path, "."))
	}
	return &ParamsProposal{Any: a, Module: module, Diff: diff, Keys: keys}, nil
}

// listDiff says what a list change adds and removes.
func listDiff(field string, before, after []any) string {
	label := func(x any) string {
		s := fmt.Sprint(x)
		if field == "active_static_precompiles" {
			if n := netspec.PrecompileName(s); n != "" {
				return s + " (" + n + ")"
			}
		}
		return s
	}
	var parts []string
	for _, x := range after {
		if !containsVal(before, x) {
			parts = append(parts, "+ "+label(x))
		}
	}
	for _, x := range before {
		if !containsVal(after, x) {
			parts = append(parts, "− "+label(x))
		}
	}
	return fmt.Sprintf("%s (%d → %d items)", strings.Join(parts, ", "), len(before), len(after))
}

// MessageJSON is the built message as the chain will read it.
func (p *ParamsProposal) MessageJSON() string {
	mt, err := protoregistry.GlobalTypes.FindMessageByURL(p.Any.TypeUrl)
	if err != nil {
		return ""
	}
	m := mt.New().Interface()
	if proto.Unmarshal(p.Any.Value, m) != nil {
		return ""
	}
	return protojson.MarshalOptions{UseProtoNames: true}.Format(m)
}

// scaleDec is a plain decimal as a legacy scaled integer (×10^18), always:
// common.DecScaled takes a whole number for an already-scaled one, which
// would turn a base fee of 1000000000 into 10^-9.
func scaleDec(v string) (string, error) {
	r, ok := new(big.Rat).SetString(v)
	if !ok {
		return "", fmt.Errorf("%q isn't a decimal", v)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))
	if !r.IsInt() {
		return "", fmt.Errorf("%q has more than 18 decimal places", v)
	}
	return r.Num().String(), nil
}
