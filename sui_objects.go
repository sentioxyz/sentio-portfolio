package portfolio

import "context"

// SuiObject is a version of a Move object. OwnerKind distinguishes
// a wallet-owned capability from a child object with the same owner address.
type SuiObject struct {
	ID, ObjectType, Owner, OwnerKind, Content string
	Version                                   uint64
	PreviousTransaction                       string
}

// SuiObjectReader reads current object state, without substituting it for a
// historical checkpoint. Missing IDs in Objects mean an explicit NotFound.
type SuiObjectReader interface {
	SuiReader
	Objects(context.Context, []string) (map[string]SuiObject, error)
	OwnedObjects(context.Context, SuiAddress, string) ([]SuiObject, error)
	DynamicFields(context.Context, string) ([]SuiObject, error)
}

// SuiObjectLineageReader follows specific object versions and their producing
// transactions to discover protocol roots and identify quotes paired with stored
// NAV. It does not reconstruct balances at historical checkpoints, or scan a
// checkpoint range.
type SuiObjectLineageReader interface {
	ObjectAtVersion(context.Context, string, uint64) (SuiObject, error)
	PreviousTransaction(context.Context, string) (string, error)
	TransactionObjectChanges(context.Context, string) ([]SuiObjectChange, error)
}

type SuiObjectChange struct {
	ID, ObjectType, OwnerKind   string
	InputVersion, OutputVersion uint64
	Created                     bool
}
