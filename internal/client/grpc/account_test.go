package grpc

import (
	"testing"

	authv1beta1 "cosmossdk.io/api/cosmos/auth/v1beta1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func marshalBaseAccount(num, seq uint64) []byte {
	b, _ := proto.Marshal(&authv1beta1.BaseAccount{
		Address:       "cosmos1abc",
		AccountNumber: num,
		Sequence:      seq,
	})
	return b
}

func TestDecodeBaseAccount(t *testing.T) {
	num, seq, err := DecodeAccount("/cosmos.auth.v1beta1.BaseAccount", marshalBaseAccount(7, 42))
	if err != nil {
		t.Fatal(err)
	}
	if num != 7 || seq != 42 {
		t.Fatalf("got (%d,%d)", num, seq)
	}
}

func TestDecodeEthAccount(t *testing.T) {
	// EthAccount { base_account = 1; code_hash = 2 }
	var eth []byte
	eth = protowire.AppendTag(eth, 1, protowire.BytesType)
	eth = protowire.AppendBytes(eth, marshalBaseAccount(9, 3))
	eth = protowire.AppendTag(eth, 2, protowire.BytesType)
	eth = protowire.AppendBytes(eth, []byte{0xab})

	num, seq, err := DecodeAccount("/ethermint.types.v1.EthAccount", eth)
	if err != nil {
		t.Fatal(err)
	}
	if num != 9 || seq != 3 {
		t.Fatalf("got (%d,%d)", num, seq)
	}
}
