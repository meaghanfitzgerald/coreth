// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package vm

import (
	"github.com/ava-labs/coreth/precompile/contract"
	"github.com/ava-labs/coreth/vmerrs"
	"github.com/ethereum/go-ethereum/common"
)

// wrappedPrecompiledContract implements StatefulPrecompiledContract by wrapping stateless native precompiled contracts
// in Ethereum.
type wrappedPrecompiledContract struct {
	p PrecompiledContract
}

func newWrappedPrecompiledContract(p PrecompiledContract) contract.StatefulPrecompiledContract {
	return &wrappedPrecompiledContract{p: p}
}

// Run implements the StatefulPrecompiledContract interface
func (w *wrappedPrecompiledContract) Run(accessibleState contract.AccessibleState, caller common.Address, addr common.Address, input []byte, suppliedGas uint64, readOnly bool) (ret []byte, remainingGas uint64, err error) {
	return RunPrecompiledContract(w.p, input, suppliedGas)
}

// RunStatefulPrecompiledContract confirms runs [precompile] with the specified parameters.
func RunStatefulPrecompiledContract(precompile contract.StatefulPrecompiledContract, accessibleState contract.AccessibleState, caller common.Address, addr common.Address, input []byte, suppliedGas uint64, readOnly bool) (ret []byte, remainingGas uint64, err error) {
	ret, remainingGas, err = precompile.Run(accessibleState, caller, addr, input, suppliedGas, readOnly)
	return ret, remainingGas, fromVMErr(err)
}

func fromVMErr(err error) error {
	switch err {
	case vmerrs.ErrExecutionReverted:
		return ErrExecutionReverted
	case vmerrs.ErrOutOfGas:
		return ErrOutOfGas
	case vmerrs.ErrInsufficientBalance:
		return ErrInsufficientBalance
	case vmerrs.ErrWriteProtection:
		return ErrWriteProtection
	}
	return err
}
