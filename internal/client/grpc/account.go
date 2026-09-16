package grpc

import (
	"fmt"

	authv1beta1 "cosmossdk.io/api/cosmos/auth/v1beta1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// DecodeAccount extracts (account_number, sequence) from an account Any.
// Handles /cosmos.auth.v1beta1.BaseAccount and /ethermint.types.v1.EthAccount
// (the cosmos-evm account type, which wraps a BaseAccount in field 1).
func DecodeAccount(typeURL string, value []byte) (num, seq uint64, err error) {
	raw := value
	if typeURL == "/ethermint.types.v1.EthAccount" ||
		typeURL == "/cosmos.evm.types.v1.EthAccount" {
		raw = fieldBytes(value, 1)
		if raw == nil {
			return 0, 0, fmt.Errorf("%s missing base_account", typeURL)
		}
	}
	var acc authv1beta1.BaseAccount
	if err := proto.Unmarshal(raw, &acc); err != nil {
		return 0, 0, fmt.Errorf("decoding %s: %w", typeURL, err)
	}
	return acc.AccountNumber, acc.Sequence, nil
}

// fieldBytes returns the bytes of a length-delimited field, or nil.
func fieldBytes(b []byte, num protowire.Number) []byte {
	for len(b) > 0 {
		n, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			return nil
		}
		b = b[l:]
		if typ == protowire.BytesType {
			v, l := protowire.ConsumeBytes(b)
			if l < 0 {
				return nil
			}
			if n == num {
				return v
			}
			b = b[l:]
		} else {
			l := protowire.ConsumeFieldValue(n, typ, b)
			if l < 0 {
				return nil
			}
			b = b[l:]
		}
	}
	return nil
}
